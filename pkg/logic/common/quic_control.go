// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// The QUIC control channel mirrors the TCP pair on UDP: an outbound conn (we dial the
// peer's UDP control endpoint) and an inbound conn (the peer dials ours), reusing the
// TCP port numbers on UDP. It carries control + metadata + the network-change signal,
// while bulk file transfer stays on TCP.
//
// The channel does NO per-connection handshake. Its keys were already derived in the
// TCP handshake payload (SEKOutboundQUIC / SEKInboundQUIC, one per direction), from the
// peer keys that were already exchanged and validated. Each QUIC stream is simply
// wrapped in a SecureConn with the matching key and a direction-specific nonce prefix
// (dialer = outbound, acceptor = inbound), so the two directions never share nonce
// space even though a QUIC stream is written by both ends.

// DialQUICControl brings up the OUTBOUND QUIC control channel to peerUDPAddr
// ("host:port"). It opens a QUIC stream and wraps it in the session's outbound
// SecureConn (SEKOutboundQUIC, dialer role). The returned net.Conn is ready to carry
// gRPC. It errors if the peer negotiated no QUIC key (older build), so the caller can
// fall back to TCP-only.
func DialQUICControl(ctx context.Context, s *session.Session, peerUDPAddr string) (net.Conn, error) {
	if s == nil {
		return nil, fmt.Errorf("quic control: nil session")
	}
	key := s.SEKOutboundQUIC
	if len(key) == 0 {
		return nil, fmt.Errorf("quic control: no outbound QUIC key (peer did not negotiate QUIC)")
	}
	raw, err := transport.QUIC().Dial(ctx, peerUDPAddr)
	if err != nil {
		return nil, fmt.Errorf("quic control dial %s: %w", peerUDPAddr, err)
	}
	return session.NewSecureConnRole(raw, key, s.NegotiatedSuite(), true), nil
}

// ListenQUICControl starts the INBOUND QUIC control channel listener on udpAddr
// ("host:port", "host:0" for an ephemeral port). Each accepted stream is wrapped in the
// session's inbound SecureConn (SEKInboundQUIC, acceptor role), ready for a gRPC server
// over a singleConnListener. It errors if the peer negotiated no QUIC key.
func ListenQUICControl(s *session.Session, udpAddr string) (net.Listener, error) {
	if s == nil {
		return nil, fmt.Errorf("quic control: nil session")
	}
	key := s.SEKInboundQUIC
	if len(key) == 0 {
		return nil, fmt.Errorf("quic control: no inbound QUIC key (peer did not negotiate QUIC)")
	}
	ln, err := transport.QUIC().Listen(udpAddr)
	if err != nil {
		return nil, fmt.Errorf("quic control listen %s: %w", udpAddr, err)
	}
	return &quicControlListener{Listener: ln, key: key, suite: s.NegotiatedSuite()}, nil
}

// quicControlListener upgrades every accepted QUIC stream to the inbound control
// SecureConn, so a plain grpc.Server can Serve it.
type quicControlListener struct {
	net.Listener
	key   []byte
	suite kbc.CipherSuite
}

func (l *quicControlListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return session.NewSecureConnRole(c, l.key, l.suite, false), nil
}

// startQUICControlChannel brings up the QUIC control pair alongside the established TCP
// session: an inbound listener on our UDP port (the same number as the TCP inbound
// port) serving the control service, and a background outbound dial to the peer's UDP
// control endpoint. It is FAIL-SOFT: any failure (UDP blocked, a peer without QUIC
// keys, a timing race) logs and leaves the session TCP-only, so it can never break
// connectivity, and the outbound dial runs in the background so it never delays connect.
func (kd *KeibiDrop) startQUICControlChannel() {
	// Idempotent: a reconnect re-runs finishConnect, so release any prior instance's
	// UDP socket + client before rebinding (otherwise the listener hits address-in-use).
	kd.stopQUICControlChannel()

	logger := kd.logger.With("component", "quic-control")
	s := kd.session
	if s == nil || len(s.SEKInboundQUIC) == 0 {
		logger.Info("peer did not negotiate a QUIC channel; staying TCP-only")
		return
	}
	ctx := kd.ctx // this generation's context; a reconnect swaps kd.ctx for a fresh one

	// Inbound listener on our UDP port (same number as the TCP inbound port).
	inAddr := net.JoinHostPort("::", strconv.Itoa(kd.inboundPort))
	if ln, err := ListenQUICControl(s, inAddr); err != nil {
		logger.Warn("QUIC control listen failed; staying TCP-only", "addr", inAddr, "error", err)
	} else {
		srv := grpc.NewServer(
			grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
			grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
		)
		cpb.RegisterControlServer(srv, quicControlService{})
		kd.mu.Lock()
		kd.quicControlServer = srv
		kd.mu.Unlock()
		go func() {
			if serveErr := srv.Serve(ln); serveErr != nil {
				logger.Debug("QUIC control server exited", "error", serveErr)
			}
		}()
		logger.Info("QUIC control channel listening", "addr", inAddr)
	}

	// Outbound dial to the peer's UDP control endpoint (same number as the TCP peer
	// port), in the background so it never delays connect.
	if kd.PeerIPv6IP == "" || len(s.SEKOutboundQUIC) == 0 {
		return
	}
	peerAddr := net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(s.PeerPort))
	go kd.dialQUICControlWithRetry(ctx, peerAddr)
}

// dialQUICControlWithRetry dials the peer's QUIC control endpoint, retrying a few times
// because the peer's listener may not be up the instant we are (both peers reach this
// just after the TCP handshake). Fail-soft: gives up to TCP-only on repeated failure.
func (kd *KeibiDrop) dialQUICControlWithRetry(ctx context.Context, peerAddr string) {
	logger := kd.logger.With("component", "quic-control")
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil {
			return
		}
		s := kd.session
		if s == nil {
			return
		}
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err := DialQUICControl(dctx, s, peerAddr)
		cancel()
		if err == nil {
			cc, cErr := grpc.NewClient("passthrough:///quic-control",
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return conn, nil }),
				grpc.WithDefaultCallOptions(
					grpc.MaxCallRecvMsgSize(config.GRPCMaxMsgSize),
					grpc.MaxCallSendMsgSize(config.GRPCMaxMsgSize),
				),
			)
			if cErr != nil {
				logger.Warn("QUIC control client build failed; staying TCP-only", "error", cErr)
				_ = conn.Close()
				return
			}
			kd.mu.Lock()
			kd.quicControlClient = cc
			kd.mu.Unlock()
			logger.Info("QUIC control channel connected", "peer", peerAddr)
			return
		}
		logger.Debug("QUIC control dial attempt failed", "attempt", attempt, "error", err)
		select {
		case <-time.After(1 * time.Second):
		case <-ctx.Done():
			return
		}
	}
	logger.Info("QUIC control channel unavailable after retries; staying TCP-only", "peer", peerAddr)
}

// stopQUICControlChannel tears down the QUIC control pair. Safe to call when it was
// never brought up (fields nil).
func (kd *KeibiDrop) stopQUICControlChannel() {
	kd.mu.Lock()
	cc := kd.quicControlClient
	srv := kd.quicControlServer
	kd.quicControlClient = nil
	kd.quicControlServer = nil
	kd.mu.Unlock()
	if cc != nil {
		_ = cc.Close()
	}
	if srv != nil {
		srv.Stop()
	}
}

// QUICControlConnected reports whether the outbound QUIC control channel is up (the
// client dialed the peer successfully). For tests and diagnostics.
func (kd *KeibiDrop) QUICControlConnected() bool {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	return kd.quicControlClient != nil
}

// PingQUICControl sends one control-plane Ping over the QUIC channel, or errors if the
// channel is not up. For tests and as the seed of the heartbeat/RTT loop.
func (kd *KeibiDrop) PingQUICControl(ctx context.Context) error {
	kd.mu.Lock()
	cc := kd.quicControlClient
	kd.mu.Unlock()
	if cc == nil {
		return fmt.Errorf("quic control channel not connected")
	}
	_, err := cpb.NewControlClient(cc).Ping(ctx, &cpb.PingRequest{})
	return err
}

// quicControlService answers the QUIC control channel's heartbeat (liveness / RTT).
// KeibiDrop metadata RPC handlers move onto this channel as routing lands.
type quicControlService struct{ cpb.UnimplementedControlServer }

func (quicControlService) Ping(context.Context, *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}
