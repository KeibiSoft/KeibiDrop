// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	cpb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto/control"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// handshakenSessionPair returns two sessions that completed the TCP handshake, so they hold
// matching independent QUIC keys (outbound.SEKOutboundQUIC == inbound.SEKInboundQUIC).
func handshakenSessionPair(t *testing.T) (outbound, inbound *session.Session) {
	t.Helper()
	logger := testkit.DiscardLogger()

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
	// The acceptor (inbound) verifies the dialer's keys against the fingerprint it holds out of band.
	inbound.ExpectedPeerFingerprint, err = outbound.OwnKeys.Fingerprint()
	require.NoError(t, err)

	oEnd, iEnd := net.Pipe()
	join := testkit.Go(func() error { return session.PerformOutboundHandshakeOnConn(outbound, oEnd) })
	require.NoError(t, session.PerformInboundHandshake(inbound, iEnd))
	require.NoError(t, join())
	_ = oEnd.Close() // the TCP handshake pipe is only needed to derive keys
	_ = iEnd.Close()

	// Reverse direction, as in the real flow (each peer dials one and accepts one). This also
	// completes key-update negotiation: a node learns PeerSupportsKeyUpdate only when it accepts.
	oEnd2, iEnd2 := net.Pipe()
	join2 := testkit.Go(func() error { return session.PerformOutboundHandshakeOnConn(inbound, oEnd2) })
	require.NoError(t, session.PerformInboundHandshake(outbound, iEnd2))
	require.NoError(t, join2())
	_ = oEnd2.Close()
	_ = iEnd2.Close()

	require.NotEmpty(t, outbound.SEKOutboundQUIC, "outbound QUIC key must be derived")
	require.Equal(t, outbound.SEKOutboundQUIC, inbound.SEKInboundQUIC, "QUIC keys must match across the pair")
	require.Equal(t, inbound.SEKOutboundQUIC, outbound.SEKInboundQUIC, "reverse-direction QUIC keys must match too")
	return outbound, inbound
}

type pingServer struct{ cpb.UnimplementedControlServer }

func (pingServer) Ping(context.Context, *cpb.PingRequest) (*cpb.PingReply, error) {
	return &cpb.PingReply{}, nil
}

// TestQUICControlChannelEndToEnd brings up the real QUIC control channel from only the
// TCP-handshake-derived keys (no per-conn handshake) and runs a gRPC RPC over it, end to end.
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

// TestQUICControlWrongKeyFails is the sad path: mismatched keys mean the SecureConn cannot
// decrypt, so the RPC fails rather than silently succeeding (QUIC/TLS connects; our layer rejects).
func TestQUICControlWrongKeyFails(t *testing.T) {
	_, bob := handshakenSessionPair(t)      // bob holds a valid inbound QUIC key
	badAlice, _ := handshakenSessionPair(t) // a different pair: badAlice's outbound key != bob's inbound key

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

// TestAnnounceHandler pins the network-change receiver: a NEW-address Announce refreshes every
// cached coordinate (TCP + UDP targets); a same-address announce is a strict no-op.
func TestAnnounceHandler(t *testing.T) {
	logger := testkit.DiscardLogger()
	s, err := session.InitSession(logger, 26002, 26001)
	require.NoError(t, err)
	s.PeerPort = 26400

	kd := newBareKD()
	kd.session = s
	kd.PeerIPv6IP = "fd00::aa"
	kd.quicPeerAddr = "[fd00::aa]:26400"

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

// quicEchoPair brings up the real QUIC control channel and echoes on the accepted conn, so
// both directions carry payload and both writers ratchet. Returns the dialer conn and accepted-conn channel.
func quicEchoPair(t *testing.T, alice, bob *session.Session) (net.Conn, <-chan net.Conn) {
	t.Helper()
	ln, err := ListenQUICControl(bob, "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			close(accepted)
			return
		}
		accepted <- c
		_ = c.SetDeadline(time.Now().Add(60 * time.Second))
		buf := make([]byte, 64<<10)
		for {
			n, rErr := c.Read(buf)
			if n > 0 {
				if _, wErr := c.Write(buf[:n]); wErr != nil {
					return
				}
			}
			if rErr != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, mc, err := DialQUICControl(ctx, alice, ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(); _ = mc.Close() })
	require.NoError(t, conn.SetDeadline(time.Now().Add(60*time.Second)))
	return conn, accepted
}

// echoStream ping-pongs total bytes through conn in chunkSize messages, verifying every
// echoed byte, and returns the worst single-chunk round-trip time.
func echoStream(t *testing.T, conn net.Conn, total, chunkSize int) time.Duration {
	t.Helper()
	payload := make([]byte, chunkSize)
	echo := make([]byte, chunkSize)
	var maxRTT time.Duration
	for sent := 0; sent < total; sent += chunkSize {
		_, err := rand.Read(payload)
		require.NoError(t, err)
		start := time.Now()
		_, err = conn.Write(payload)
		require.NoError(t, err)
		_, err = io.ReadFull(conn, echo)
		require.NoError(t, err)
		if d := time.Since(start); d > maxRTT {
			maxRTT = d
		}
		require.True(t, bytes.Equal(payload, echo), "echo mismatch at offset %d", sent)
	}
	return maxRTT
}

// TestQUICControlChannelRatchetsAcrossEpochs is the seamless-rekey proof: streaming enough data
// fires the in-band ratchet many times per direction, verifying every byte round-trips, no chunk
// stalls, and both writer epochs advanced (not stuck at epoch 0, the bug this guards against).
func TestQUICControlChannelRatchetsAcrossEpochs(t *testing.T) {
	alice, bob := handshakenSessionPair(t)
	require.True(t, alice.UseKeyUpdate(), "full handshake pair must negotiate the in-band ratchet (dialer side)")
	require.True(t, bob.UseKeyUpdate(), "full handshake pair must negotiate the in-band ratchet (acceptor side)")

	// Drop the byte threshold so a 2 MiB stream crosses ~32 epochs per direction. Direct
	// assignment bypasses the env floor; restored after the test (no parallel tests touch these vars).
	origBytes := session.RekeyBytesThreshold
	session.RekeyBytesThreshold = 64 << 10
	t.Cleanup(func() { session.RekeyBytesThreshold = origBytes })

	conn, accepted := quicEchoPair(t, alice, bob)
	maxRTT := echoStream(t, conn, 2<<20, 16<<10)

	dialSC, ok := conn.(*session.SecureConn)
	require.True(t, ok, "DialQUICControl must return a *session.SecureConn")
	require.GreaterOrEqual(t, dialSC.WriterEpoch(), uint16(8),
		"dialer-side QUIC conn must have ratcheted across many epochs")

	var srvConn net.Conn
	select {
	case srvConn = <-accepted:
		require.NotNil(t, srvConn)
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the QUIC control conn")
	}
	srvSC, ok := srvConn.(*session.SecureConn)
	require.True(t, ok, "accepted conn must be a *session.SecureConn")
	require.GreaterOrEqual(t, srvSC.WriterEpoch(), uint16(8),
		"acceptor-side QUIC conn must have ratcheted across many epochs")

	// Seamlessness: an epoch bump rides the next record and must never pause the stream.
	// Generous bound so CI noise cannot flake it, while a real stall would blow far past.
	require.Less(t, maxRTT, 2*time.Second, "a rekey must never stall the stream")
	t.Logf("QUIC ratchet proof: dialer epoch %d, acceptor epoch %d, worst chunk RTT %v",
		dialSC.WriterEpoch(), srvSC.WriterEpoch(), maxRTT)
}

// TestQUICControlChannelStaysEpochZeroWithoutNegotiation is the gate proof: without the
// negotiated capability the QUIC conns must stay at epoch 0 for old-peer interop while traffic flows.
func TestQUICControlChannelStaysEpochZeroWithoutNegotiation(t *testing.T) {
	alice, bob := handshakenSessionPair(t)
	alice.PeerSupportsKeyUpdate = false
	bob.PeerSupportsKeyUpdate = false
	require.False(t, alice.UseKeyUpdate())
	require.False(t, bob.UseKeyUpdate())

	// Tiny threshold that WOULD force ratchets if the gate failed open.
	origBytes := session.RekeyBytesThreshold
	session.RekeyBytesThreshold = 64 << 10
	t.Cleanup(func() { session.RekeyBytesThreshold = origBytes })

	conn, accepted := quicEchoPair(t, alice, bob)
	_ = echoStream(t, conn, 512<<10, 16<<10)

	dialSC := conn.(*session.SecureConn)
	require.Equal(t, uint16(0), dialSC.WriterEpoch(), "without negotiation the dialer conn must stay at epoch 0")
	select {
	case srvConn := <-accepted:
		require.NotNil(t, srvConn)
		require.Equal(t, uint16(0), srvConn.(*session.SecureConn).WriterEpoch(),
			"without negotiation the acceptor conn must stay at epoch 0")
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the QUIC control conn")
	}
}

// foldRelayClient carries a fold round's Rekey RPC straight to the responder session; only Rekey is implemented.
type foldRelayClient struct {
	bindings.KeibiServiceClient
	responder *session.Session
}

func (c foldRelayClient) Rekey(_ context.Context, req *bindings.RekeyRequest, _ ...grpc.CallOption) (*bindings.RekeyResponse, error) {
	return c.responder.RespondToFold(req)
}

// TestQUICControlChannelFoldCoversQUICLane proves a fold round staged via ExtraFoldConns lands
// on both peers' real QUIC control conns. With the byte threshold (2^62) and time cadence (60s)
// out of reach, a plain ratchet is impossible, so an epoch advance proves the fold applied.
func TestQUICControlChannelFoldCoversQUICLane(t *testing.T) {
	alice, bob := handshakenSessionPair(t)
	origBytes := session.RekeyBytesThreshold
	session.RekeyBytesThreshold = 1 << 62
	t.Cleanup(func() { session.RekeyBytesThreshold = origBytes })

	conn, accepted := quicEchoPair(t, alice, bob)
	_ = echoStream(t, conn, 16<<10, 16<<10) // force the accept; epoch-0 baseline
	var srvConn net.Conn
	select {
	case srvConn = <-accepted:
		require.NotNil(t, srvConn)
	case <-time.After(5 * time.Second):
		t.Fatal("server never accepted the QUIC control conn")
	}
	dialSC := conn.(*session.SecureConn)
	srvSC := srvConn.(*session.SecureConn)
	require.Equal(t, uint16(0), dialSC.WriterEpoch(), "baseline: no rotation before the fold")
	require.Equal(t, uint16(0), srvSC.WriterEpoch(), "baseline: no rotation before the fold")

	// Wire the lane into fold staging exactly as production does (kd.quicFoldConns).
	alice.ExtraFoldConns = func() []*session.SecureConn { return []*session.SecureConn{dialSC} }
	bob.ExtraFoldConns = func() []*session.SecureConn { return []*session.SecureConn{srvSC} }

	initiator, responder := alice, bob
	if !alice.IsFoldInitiator() {
		initiator, responder = bob, alice
	}
	require.NoError(t, initiator.InitiateFoldVia(context.Background(), foldRelayClient{responder: responder}))

	// Two ping-pongs: enough for both writers to fold regardless of which end initiated
	// (a responder's writer folds only after its own reader followed the initiator's).
	_ = echoStream(t, conn, 2*(16<<10), 16<<10)
	require.Equal(t, uint16(1), dialSC.WriterEpoch(),
		"dial-side QUIC conn must have FOLDED (plain ratchet impossible here)")
	require.Equal(t, uint16(1), srvSC.WriterEpoch(),
		"accepted-side QUIC conn must have FOLDED")
}

// TestQUICListenerAcceptHookFires pins the accept path: wrap in a keyed SecureConn,
// retain for fold/epoch access, and fire the QUIC-conn-up fold trigger.
func TestQUICListenerAcceptHookFires(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	fired := 0
	l := &quicControlListener{
		Listener:  NewSingleConnListener(c1),
		key:       make([]byte, 32),
		suite:     kbc.CipherChaCha20,
		keyUpdate: true,
		onAccept:  func() { fired++ },
	}
	got, err := l.Accept()
	require.NoError(t, err)
	sc, ok := got.(*session.SecureConn)
	require.True(t, ok, "accepted conn must be a SecureConn")
	require.Equal(t, 1, fired, "accept must fire the fold trigger")
	require.Same(t, sc, l.LastAccepted(), "accepted conn must be retained")
}

// TestQUICControlRefusedWithoutKey proves fail-soft: with no negotiated QUIC key (older
// peer), bring-up returns an error instead of proceeding insecurely.
func TestQUICControlRefusedWithoutKey(t *testing.T) {
	logger := testkit.DiscardLogger()
	s, err := session.InitSession(logger, 26002, 26001)
	require.NoError(t, err)

	_, _, err = DialQUICControl(context.Background(), s, "127.0.0.1:1")
	require.Error(t, err)
	_, err = ListenQUICControl(s, "127.0.0.1:0")
	require.Error(t, err)
}
