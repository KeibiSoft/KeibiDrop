// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	chain "github.com/KeibiSoft/go-fp/immutable"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// channelMagicByte is the first byte sent on a new data channel TCP connection.
// Used to distinguish data channel connections from handshake connections.
// Value 0xC4 chosen to not conflict with JSON '{' (0x7B) used in handshake.
const channelMagicByte byte = 0xC4

// ChannelHandle holds references to a fully set up data channel.
type ChannelHandle struct {
	ChannelID  uint32
	Conn       *SecureConn
	GRPCServer *grpc.Server
	GRPCClient bindings.KeibiServiceClient
	ClientConn *grpc.ClientConn
	logger     *slog.Logger
}

// Close tears down the data channel.
func (ch *ChannelHandle) Close() {
	if ch.GRPCServer != nil {
		ch.GRPCServer.Stop()
	}
	if ch.ClientConn != nil {
		ch.ClientConn.Close()
	}
	if ch.Conn != nil {
		ch.Conn.Close()
	}
}

// NegotiatedChannel holds the result of the NegotiateChannel RPC exchange.
type NegotiatedChannel struct {
	ChannelID  uint32
	ChannelKey []byte // Derived symmetric key for this channel.
	PeerAddr   string
	Logger     *slog.Logger
}

// ConnectedChannel holds a SecureConn ready for gRPC.
type ConnectedChannel struct {
	NegotiatedChannel
	Conn *SecureConn
}

// --- Pipeline steps (composable via go-fp Chain) ---

// NegotiateStep exchanges seeds with peer via the control gRPC and derives a channel key.
func NegotiateStep(controlClient bindings.KeibiServiceClient, sessionKey []byte, channelID uint32, logger *slog.Logger) chain.Chain[NegotiatedChannel] {
	return chain.LiftResult(func() (NegotiatedChannel, error) {
		// Generate random seed.
		seed := make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, seed); err != nil {
			return NegotiatedChannel{}, fmt.Errorf("generate seed: %w", err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		resp, err := controlClient.NegotiateChannel(ctx, &bindings.NegotiateChannelRequest{
			ChannelId: channelID,
			KeySeed:   seed,
		})
		if err != nil {
			return NegotiatedChannel{}, fmt.Errorf("negotiate RPC: %w", err)
		}
		if !resp.Accepted {
			return NegotiatedChannel{}, fmt.Errorf("peer rejected channel %d", channelID)
		}

		// Derive channel key: HKDF(sessionKey, ourSeed || peerSeed || channelID).
		channelIDBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(channelIDBytes, channelID)
		info := append(append(seed, resp.KeySeed...), channelIDBytes...)
		channelKey, err := kbc.DeriveChaCha20Key(sessionKey, info)
		if err != nil {
			return NegotiatedChannel{}, fmt.Errorf("derive channel key: %w", err)
		}

		logger.Info("Channel negotiated", "channelID", channelID)
		return NegotiatedChannel{
			ChannelID:  channelID,
			ChannelKey: channelKey,
			Logger:     logger,
		}, nil
	})
}

// ConnectStep dials the peer and establishes an encrypted TCP connection for the channel.
func ConnectStep(peerAddr string, dialTimeout time.Duration) func(NegotiatedChannel) chain.Chain[ConnectedChannel] {
	return func(neg NegotiatedChannel) chain.Chain[ConnectedChannel] {
		return chain.LiftResult(func() (ConnectedChannel, error) {
			conn, err := net.DialTimeout("tcp", peerAddr, dialTimeout)
			if err != nil {
				return ConnectedChannel{}, fmt.Errorf("dial peer for channel %d: %w", neg.ChannelID, err)
			}

			// Send channel header: [magic_byte][channel_id_uint32] = 5 bytes.
			header := make([]byte, 5)
			header[0] = channelMagicByte
			binary.BigEndian.PutUint32(header[1:], neg.ChannelID)
			if _, err := conn.Write(header); err != nil {
				conn.Close()
				return ConnectedChannel{}, fmt.Errorf("send channel header: %w", err)
			}

			secureConn := NewSecureConn(conn, neg.ChannelKey)
			neg.Logger.Info("Channel connected", "channelID", neg.ChannelID)

			return ConnectedChannel{
				NegotiatedChannel: neg,
				Conn:              secureConn,
			}, nil
		})
	}
}

// StartGRPCStep starts a gRPC server and client on the channel's SecureConn.
func StartGRPCStep(svc bindings.KeibiServiceServer) func(ConnectedChannel) chain.Chain[ChannelHandle] {
	return func(cc ConnectedChannel) chain.Chain[ChannelHandle] {
		return chain.LiftResult(func() (ChannelHandle, error) {
			// Start gRPC server on this channel's inbound connection.
			srv := grpc.NewServer(
				grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
				grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
			)
			bindings.RegisterKeibiServiceServer(srv, svc)

			ln := newSingleConnListener(cc.Conn)
			go func() {
				if err := srv.Serve(ln); err != nil {
					cc.Logger.Error("Data channel gRPC server exited", "channelID", cc.ChannelID, "error", err)
				}
			}()

			// Connect gRPC client through the same SecureConn.
			clientConn, err := grpc.Dial(
				"keibichannel",
				grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
					return cc.Conn, nil
				}),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(config.GRPCMaxMsgSize),
					grpc.MaxCallSendMsgSize(config.GRPCMaxMsgSize),
				),
			)
			if err != nil {
				srv.Stop()
				return ChannelHandle{}, fmt.Errorf("dial data channel gRPC: %w", err)
			}

			client := bindings.NewKeibiServiceClient(clientConn)
			cc.Logger.Info("Channel gRPC ready", "channelID", cc.ChannelID)

			return ChannelHandle{
				ChannelID:  cc.ChannelID,
				Conn:       cc.Conn,
				GRPCServer: srv,
				GRPCClient: client,
				ClientConn: clientConn,
				logger:     cc.Logger,
			}, nil
		})
	}
}

// OpenChannel composes the full pipeline: negotiate → connect → start gRPC.
func OpenChannel(
	controlClient bindings.KeibiServiceClient,
	sessionKey []byte,
	channelID uint32,
	peerAddr string,
	svc bindings.KeibiServiceServer,
	logger *slog.Logger,
) chain.Chain[ChannelHandle] {
	negotiated := NegotiateStep(controlClient, sessionKey, channelID, logger)
	connected := chain.Bind(negotiated, ConnectStep(peerAddr, 3*time.Second))
	return chain.Bind(connected, StartGRPCStep(svc))
}

// OpenDataChannels opens N data channels using the composable pipeline.
// On partial failure, already-opened channels are closed.
func OpenDataChannels(
	controlClient bindings.KeibiServiceClient,
	sessionKey []byte,
	peerAddr string,
	svc bindings.KeibiServiceServer,
	n int,
	logger *slog.Logger,
) ([]ChannelHandle, error) {
	handles := make([]ChannelHandle, 0, n)

	for i := 0; i < n; i++ {
		channelID := uint32(i + 1)
		result := OpenChannel(controlClient, sessionKey, channelID, peerAddr, svc, logger)

		handle, err := result.Result()
		if err != nil {
			logger.Error("Failed to open data channel", "channelID", channelID, "error", err)
			for _, h := range handles {
				h.Close()
			}
			return nil, fmt.Errorf("channel %d: %w", channelID, err)
		}

		handles = append(handles, handle)
	}

	logger.Info("All data channels opened", "count", n)
	return handles, nil
}

// newSingleConnListener wraps a single net.Conn as a net.Listener.
// Used to serve one gRPC connection per data channel.
func newSingleConnListener(conn net.Conn) net.Listener {
	return &singleChannelListener{
		conn: conn,
		addr: conn.LocalAddr(),
		done: make(chan struct{}),
	}
}

type singleChannelListener struct {
	conn  net.Conn
	addr  net.Addr
	done  chan struct{}
	used  bool
}

func (l *singleChannelListener) Accept() (net.Conn, error) {
	if l.used {
		<-l.done
		return nil, io.EOF
	}
	l.used = true
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (l *singleChannelListener) Close() error {
	select {
	case <-l.done:
	default:
		close(l.done)
	}
	return nil
}

func (l *singleChannelListener) Addr() net.Addr {
	return l.addr
}

// IsChannelConnection reads the first byte of a new TCP connection to
// determine if it's a data channel (magic byte) or a handshake.
// Returns (channelID, true) for data channels, (0, false) for handshakes.
// The connection is left positioned after the 5-byte header if it's a channel.
func IsChannelConnection(conn net.Conn) (uint32, bool, error) {
	// Peek at first byte with a short deadline.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer conn.SetReadDeadline(time.Time{})

	header := make([]byte, 5)
	_, err := io.ReadFull(conn, header)
	if err != nil {
		return 0, false, err
	}

	if header[0] == channelMagicByte {
		channelID := binary.BigEndian.Uint32(header[1:])
		return channelID, true, nil
	}

	// Not a channel — this is a handshake. But we already consumed 5 bytes.
	// The caller needs to handle this (prepend the bytes back).
	return 0, false, nil
}

// AcceptChannelConnection accepts a data channel TCP connection on the listener,
// reads the channel header, and wraps it in a SecureConn with the derived key.
func AcceptChannelConnection(
	listener net.Listener,
	channelKey []byte,
	timeout time.Duration,
) (*SecureConn, uint32, error) {
	if tcpL, ok := listener.(*net.TCPListener); ok {
		tcpL.SetDeadline(time.Now().Add(timeout))
	}

	conn, err := listener.Accept()
	if err != nil {
		return nil, 0, fmt.Errorf("accept: %w", err)
	}

	channelID, isChannel, err := IsChannelConnection(conn)
	if err != nil {
		conn.Close()
		return nil, 0, fmt.Errorf("read channel header: %w", err)
	}
	if !isChannel {
		conn.Close()
		return nil, 0, fmt.Errorf("expected channel connection, got handshake")
	}

	secureConn := NewSecureConn(conn, channelKey)
	return secureConn, channelID, nil
}
