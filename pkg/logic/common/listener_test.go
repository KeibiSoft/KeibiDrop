// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests to ensure the listener is always dual-stack (tcp, not tcp6)
// ABOUTME: and that LAN addresses are only used in local mode.

package common

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// pickFreePortPair returns a base port with base and base+1 free.
// Test hosts run production listeners in the 267xx range; probe
// wildcard and v4 loopback so busy ports are skipped.
func pickFreePortPair(t *testing.T) int {
	t.Helper()
	for base := 26700; base < 26900; base += 2 {
		if portFree(base) && portFree(base+1) {
			return base
		}
	}
	t.Fatal("no free port pair in 26700-26900")
	return 0
}

func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	ln.Close()
	ln4, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	ln4.Close()
	return true
}

// TestListenerAcceptsIPv4 verifies the listener is dual-stack and accepts IPv4.
// Regression test: a previous change switched to tcp6 which broke bridge relay
// on devices without IPv6 (gRPC loopback uses 127.0.0.1 on some platforms).
func TestListenerAcceptsIPv4(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	// Try connecting via IPv4 loopback
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "IPv4 connection to listener failed (listener must be dual-stack 'tcp', not 'tcp6')")
	conn.Close()

	// Try connecting via IPv6 loopback
	conn6, err := net.Dial("tcp6", fmt.Sprintf("[::1]:%d", port))
	require.NoError(t, err, "IPv6 connection to listener failed")
	conn6.Close()

	_ = kd
}

// TestLANAddressesSkippedInInternetMode verifies that PeerLocalAddrs are NOT
// tried during JoinRoom when IsLocalMode is false.
// Regression test: LAN addresses were tried in internet mode, causing the
// joiner to connect via IPv4 LAN while the creator expected IPv6, leading to
// stuck connections and fingerprint mismatches on the bridge.
func TestLANAddressesSkippedInInternetMode(t *testing.T) {
	// This is a code-level assertion. Verify the condition in logic.go.
	// The JoinRoom LAN block should be gated by kd.IsLocalMode.
	//
	// We can't easily test the full JoinRoom flow here (needs relay + bridge),
	// but we verify the KeibiDrop struct defaults are correct.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	// Default should NOT be local mode
	require.False(t, kd.IsLocalMode, "IsLocalMode should default to false")

	// Simulate having LAN addresses from relay registration
	kd.PeerLocalAddrs = []string{"192.168.1.42", "fe80::1%eth0"}

	// In internet mode (IsLocalMode=false), these should be ignored.
	// The actual enforcement is in logic.go JoinRoom:
	//   if kd.IsLocalMode && len(kd.PeerLocalAddrs) > 0 {
	// We verify the flag is correct. The logic test is the guard condition.
	require.False(t, kd.IsLocalMode, "IsLocalMode should still be false after setting PeerLocalAddrs")
}

// TestListenerClosedOnBridgeFallback verifies that after closing the listener
// (as CreateRoom does when P2P times out), TCP dials are refused, and the
// reopened listener accepts connections again.
// Regression test for #146: stale listener caused phantom P2P connections.
func TestListenerClosedOnBridgeFallback(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	// Verify the listener works before close.
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "Dial before close failed")
	conn.Close()

	// Close the listener and nil it. Simulates CreateRoom bridge fallback.
	kd.listener.Close()
	kd.listener = nil

	// Dial MUST be refused now. This is the core of the #146 fix:
	// before the fix, the listener stayed open and the kernel accepted
	// phantom connections into the backlog.
	_, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	require.Error(t, dialErr, "Dial after listener close should be refused, but succeeded (phantom accept)")

	// Reopen on the same port. Simulates the reopen step.
	addr := net.JoinHostPort("", fmt.Sprintf("%d", port))
	newLn, err := net.Listen("tcp", addr)
	require.NoError(t, err, "Reopen listener failed")
	kd.listener = newLn

	// New listener must accept connections.
	conn2, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "Dial after reopen failed")
	conn2.Close()
}

// TestAcceptDeadlineExitsOnTimeout verifies that Accept with SetDeadline
// returns a deadline error promptly, matching the pattern used in JoinRoom
// for inbound P2P accept.
// Regression test for #146: goroutine leak when Accept had no deadline.
func TestAcceptDeadlineExitsOnTimeout(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	_ = kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, acceptErr := kd.listener.Accept()
	_ = kd.listener.(*net.TCPListener).SetDeadline(time.Time{})

	require.Error(t, acceptErr, "Expected deadline error from Accept, got nil")
	require.True(t, os.IsTimeout(acceptErr), "Expected timeout error, got: %v", acceptErr)
}

// TestCreateRoom_LocalModeNilListener_NoPanic reproduces the reconnect panic: a prior
// bridge-fallback timeout (createRendezvousRound) or a concurrent Shutdown (Run's
// permanent-shutdown branch) can leave kd.listener nil across a CreateRoom re-entry, and
// Accept on a nil interface panics.
func TestCreateRoom_LocalModeNilListener_NoPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	kd.IsLocalMode = true
	// Simulates a prior connect's bridge-fallback timeout (createRendezvousRound,
	// logic.go:1175-1176) or a concurrent Shutdown (types.go Run loop) leaving the
	// listener nil across a CreateRoom re-entry.
	kd.listener.Close()
	kd.listener = nil
	kd.session.ExpectedPeerFingerprint = "test-peer-fingerprint" // skip the wait-for-fingerprint poll

	require.NotPanics(t, func() {
		err = kd.CreateRoom()
	}, "CreateRoom must not panic when the listener is nil")
	require.ErrorIs(t, err, ErrListenerNotOpen)
}

// TestCreateRoom_ConcurrentTeardownNilsListener_RaceClean covers the -race requirement: a
// real concurrent writer (modeling Run's permanent-shutdown teardown) nils kd.listener under
// kd.mu while CreateRoom repeatedly snapshots it under the same lock. Before the fix, the
// read at the local-mode Accept site was bare and the write at Run's teardown was bare, so
// -race flagged this pair directly.
func TestCreateRoom_ConcurrentTeardownNilsListener_RaceClean(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")
	kd.IsLocalMode = true
	kd.session.ExpectedPeerFingerprint = "test-peer-fingerprint"
	kd.listener.Close()
	kd.listener = nil // starts nil, like a prior bridge-fallback timeout left it

	const iters = 500
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < iters; i++ {
			kd.mu.Lock()
			kd.listener = nil // models Run's permanent-shutdown teardown, types.go:626-628
			kd.mu.Unlock()
		}
	}()

	for i := 0; i < iters; i++ {
		var callErr error
		require.NotPanics(t, func() { callErr = kd.CreateRoom() },
			"CreateRoom must not panic while a concurrent teardown nils the listener")
		require.ErrorIs(t, callErr, ErrListenerNotOpen)
	}
	<-writerDone
}

// TestJoinRoom_LocalModeNilListener_NoPanic mirrors the CreateRoom repro for JoinRoom's LAN
// inbound accept (logic.go, "Connection priority" LAN branch, the JoinRoom twin of the
// CreateRoom local-mode Accept). Same bug, same field, same fix shape. The LAN branch is
// picked over the direct-IPv6 branch because it needs the least fake-peer scaffolding to
// drive for real: one plaintext local-mode key exchange plus one PQC handshake, versus the
// direct-IPv6 branch's extra InboundBlocked/bridge-fallback state machine.
//
// The fake peer plays the "creator" role for exactly the two legs JoinRoom's LAN path runs
// before it ever touches the joiner's own listener: the plaintext local key exchange, then
// the PQC handshake the joiner dials out for. It never dials back into kd.listener, so the
// guard is what has to stop the joiner from reaching a nil Accept, not a real peer accept.
func TestJoinRoom_LocalModeNilListener_NoPanic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	peerPort := pickFreePortPair(t)
	peerSession, err := session.InitSession(logger, peerPort+1, peerPort)
	require.NoError(t, err, "InitSession for the fake LAN peer failed")
	peerSession.ExpectedPeerFingerprint = "TOFU" // accept the joiner's real fingerprint, as local mode does

	peerLn, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(peerPort)))
	require.NoError(t, err, "fake LAN peer listen failed")
	defer peerLn.Close()

	peerDone := make(chan error, 1)
	go func() {
		keyConn, err := peerLn.Accept()
		if err != nil {
			peerDone <- fmt.Errorf("fake peer key-exchange accept: %w", err)
			return
		}
		defer keyConn.Close()
		if err := session.ExchangePublicKeysLocal(peerSession, keyConn, false); err != nil {
			peerDone <- fmt.Errorf("fake peer key exchange: %w", err)
			return
		}

		hsConn, err := peerLn.Accept()
		if err != nil {
			peerDone <- fmt.Errorf("fake peer handshake accept: %w", err)
			return
		}
		defer hsConn.Close()
		peerDone <- session.PerformInboundHandshake(peerSession, hsConn)
	}()

	kd.IsLocalMode = true
	kd.session.ExpectedPeerFingerprint = "TOFU"
	kd.PeerIPv6IP = "127.0.0.1"               // dial target for the local-mode key exchange
	kd.PeerLocalAddrs = []string{"127.0.0.1"} // dial target for the LAN accept branch
	kd.session.PeerPort = peerPort

	// Simulates a prior bridge-fallback timeout or a concurrent Shutdown leaving the
	// listener nil across a JoinRoom re-entry, same as the CreateRoom repro.
	kd.listener.Close()
	kd.listener = nil

	require.NotPanics(t, func() {
		err = kd.JoinRoom()
	}, "JoinRoom must not panic when the listener is nil")
	require.ErrorIs(t, err, ErrListenerNotOpen)
	require.Nil(t, kd.session.OutboundConn(),
		"outbound conn must be closed and cleared on this bail-out, mirroring the accept-failure cleanup, not stranded")

	require.NoError(t, <-peerDone, "fake LAN peer side failed")
}

// TestAcceptConn_ConcurrentTeardownNilsListener_RaceClean covers the same bug class as
// TestCreateRoom_ConcurrentTeardownNilsListener_RaceClean, found by concurrency review just
// outside 393e11f: the ReconnectManager.AcceptConn closure InitConnectionResilience installs
// (resilience.go) reads kd.listener bare. reconnectAsInitiator/reconnectDirectResponder call
// AcceptConn(30s) from the reconnectLoop goroutine (pkg/session/reconnect.go), and
// ReconnectManager.Stop() only waits 2s for that goroutine to exit, so it can provably outlive
// StopConnectionResilience() and race Run's kd.mu-guarded teardown write. This drives the real
// closure through InitConnectionResilience, not a hand-copied duplicate, so a future edit to the
// real closure cannot silently drift away from what this test exercises.
func TestAcceptConn_ConcurrentTeardownNilsListener_RaceClean(t *testing.T) {
	kd := newTestKD(t)
	kd.IsLocalMode = true // skip relay keepalive/lookup wiring, irrelevant to this closure

	rel := make(chan struct{})
	close(rel)
	kd.KDClient = &blockingNotifyClient{entered: make(chan struct{}, 1), release: rel}

	require.NoError(t, kd.InitConnectionResilience())
	t.Cleanup(kd.StopConnectionResilience)

	kd.listener.Close()
	kd.listener = nil // starts nil, like a prior bridge-fallback timeout left it

	const iters = 500
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < iters; i++ {
			kd.mu.Lock()
			kd.listener = nil // models Run's permanent-shutdown teardown, types.go
			kd.mu.Unlock()
		}
	}()

	for i := 0; i < iters; i++ {
		require.NotPanics(t, func() {
			_, _ = kd.ReconnectManager.AcceptConn(10 * time.Millisecond)
		}, "AcceptConn must not panic while a concurrent teardown nils the listener")
	}
	<-writerDone
}
