// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// handshakenSessionPair returns two sessions that have completed the TCP handshake, so
// they hold matching, independent QUIC keys: the outbound session's SEKOutboundQUIC ==
// the inbound session's SEKInboundQUIC.
func handshakenSessionPair(t *testing.T) (outbound, inbound *session.Session) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	outbound, err := session.InitSession(logger, 26002, 26001)
	require.NoError(t, err)
	inbound, err = session.InitSession(logger, 26004, 26003)
	require.NoError(t, err)

	// Peer keys are already exchanged + validated before the PQC handshake runs.
	inboundPeer, err := kbc.ParsePeerKeys(map[string][]byte{
		"x25519": inbound.OwnKeys.X25519Public.Bytes(),
		"mlkem":  inbound.OwnKeys.MlKemPublic.Bytes(),
	})
	require.NoError(t, err)
	outbound.PeerPubKeys = inboundPeer
	outbound.ExpectedPeerFingerprint, err = inbound.OwnKeys.Fingerprint()
	require.NoError(t, err)
	// The acceptor (inbound) verifies the dialer's keys against the fingerprint it
	// already holds out of band.
	inbound.ExpectedPeerFingerprint, err = outbound.OwnKeys.Fingerprint()
	require.NoError(t, err)

	oEnd, iEnd := net.Pipe()
	errCh := make(chan error, 1)
	go func() { errCh <- session.PerformOutboundHandshakeOnConn(outbound, oEnd) }()
	require.NoError(t, session.PerformInboundHandshake(inbound, iEnd))
	require.NoError(t, <-errCh)
	_ = oEnd.Close() // the TCP handshake pipe is only needed to derive keys
	_ = iEnd.Close()

	require.NotEmpty(t, outbound.SEKOutboundQUIC, "outbound QUIC key must be derived")
	require.Equal(t, outbound.SEKOutboundQUIC, inbound.SEKInboundQUIC, "QUIC keys must match across the pair")
	return outbound, inbound
}

type pingServer struct{ cpb.UnimplementedControlServer }

func (pingServer) Ping(context.Context, *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}

// TestQUICControlChannelEndToEnd brings up the real QUIC control channel between two
// sessions using ONLY the keys derived in the TCP handshake (no per-conn handshake),
// and runs a gRPC RPC over it. This proves the whole mechanism end to end: extra seeds
// in the handshake payload -> independent QUIC keys -> role-keyed SecureConn over a real
// QUIC stream -> working gRPC. Control + metadata ride this; file transfer stays on TCP.
func TestQUICControlChannelEndToEnd(t *testing.T) {
	alice, bob := handshakenSessionPair(t) // alice = outbound (dials), bob = inbound (listens)

	ln, err := ListenQUICControl(bob, "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	srv := grpc.NewServer()
	cpb.RegisterControlServer(srv, pingServer{})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, mc, err := DialQUICControl(ctx, alice, ln.Addr().String())
	require.NoError(t, err)
	defer mc.Close()

	cc, err := grpc.NewClient("passthrough:///quic-control",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return conn, nil }))
	require.NoError(t, err)
	defer cc.Close()

	_, err = cpb.NewControlClient(cc).Ping(ctx, &cpb.PingRequest{})
	require.NoError(t, err, "Ping over the session-keyed QUIC control channel must succeed")
}

// TestQUICControlWrongKeyFails is the sad path: if the two ends are keyed differently
// (a wrong or rotated key), the SecureConn cannot decrypt, so the RPC fails rather than
// silently succeeding. The QUIC/TLS layer connects, but our post-quantum layer rejects.
func TestQUICControlWrongKeyFails(t *testing.T) {
	_, bob := handshakenSessionPair(t)          // bob holds a valid inbound QUIC key
	badAlice, _ := handshakenSessionPair(t)      // a different pair: badAlice's outbound key != bob's inbound key

	ln, err := ListenQUICControl(bob, "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	srv := grpc.NewServer()
	cpb.RegisterControlServer(srv, pingServer{})
	go func() { _ = srv.Serve(ln) }()
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, mc, err := DialQUICControl(ctx, badAlice, ln.Addr().String())
	require.NoError(t, err) // QUIC dial + stream open succeed; the mismatch bites at the record layer
	defer mc.Close()
	cc, err := grpc.NewClient("passthrough:///quic-control",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return conn, nil }))
	require.NoError(t, err)
	defer cc.Close()

	pctx, pcancel := context.WithTimeout(ctx, 4*time.Second)
	defer pcancel()
	_, err = cpb.NewControlClient(cc).Ping(pctx, &cpb.PingRequest{})
	require.Error(t, err, "Ping with mismatched QUIC keys must fail, not silently succeed")
}

// TestAnnounceHandler pins the network-change receiver: a peer's Announce with a NEW
// address refreshes every cached coordinate (TCP reconnect target, UDP control target);
// a same-address announce is a strict no-op. Reconnect kicking is nil-guarded here
// (ReconnectManager unset), exercised by the live-flow tests.
func TestAnnounceHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := session.InitSession(logger, 26002, 26001)
	require.NoError(t, err)
	s.PeerPort = 26400

	kd := &KeibiDrop{logger: logger, session: s, PeerIPv6IP: "fd00::aa", quicPeerAddr: "[fd00::aa]:26400"}

	svc := quicControlService{kd: kd}

	// Same address: strict no-op.
	_, err = svc.Announce(context.Background(), &cpb.AnnounceRequest{Ip: "fd00::aa", TcpPort: 26400})
	require.NoError(t, err)
	require.Equal(t, "fd00::aa", kd.PeerIPv6IP)
	require.Equal(t, "[fd00::aa]:26400", kd.quicPeerAddr)

	// New address: every cached coordinate moves.
	_, err = svc.Announce(context.Background(), &cpb.AnnounceRequest{Ip: "fd00::bb", TcpPort: 26400})
	require.NoError(t, err)
	require.Equal(t, "fd00::bb", kd.PeerIPv6IP, "TCP reconnect target must move")
	require.Equal(t, "[fd00::bb]:26400", kd.quicPeerAddr, "UDP control target must move (maintainer redials the new addr)")

	// Garbage announces are ignored.
	_, err = svc.Announce(context.Background(), &cpb.AnnounceRequest{Ip: "", TcpPort: 0})
	require.NoError(t, err)
	require.Equal(t, "fd00::bb", kd.PeerIPv6IP)
}

// TestQUICControlRefusedWithoutKey proves fail-soft: with no negotiated QUIC key (older
// peer), bring-up returns an error instead of proceeding insecurely.
func TestQUICControlRefusedWithoutKey(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := session.InitSession(logger, 26002, 26001)
	require.NoError(t, err)

	_, _, err = DialQUICControl(context.Background(), s, "127.0.0.1:1")
	require.Error(t, err)
	_, err = ListenQUICControl(s, "127.0.0.1:0")
	require.Error(t, err)
}
