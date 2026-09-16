// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// A second CreateRoom, JoinRoom or Connect while one is in flight is refused
// and opens nothing. Seen 2026-09-16 on 0.4.8: auto-connect had the creator's
// CreateRoom waiting on its bridge leg, a click on Connect reached CreateRoom
// again, the second leg carried the same pair1 token, the bridge paired the
// creator with itself and the real joiner read EOF. Every frontend guards its
// own button and none of them sees the engine's auto-connect, so the guard
// lives in the engine and this file drives the real CreateRoom through it.
//
// The guard makes Cancel matter: a retry lands only once the cancelled
// connect has unwound, so a cancel must end every long wait at once, and the
// retry that arrives during the unwind must be admitted, not refused.

package common

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// registerOnlyRelay answers the one call CreateRoom makes before its rounds,
// the room registration, and knows nothing else: the reachability probe gets
// a 404, which leaves the blocked mark as the test set it, and a fetch gets
// the 404 a joiner sees while its peer has not registered yet.
func registerOnlyRelay(t *testing.T) *url.URL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u
}

// joinOnce runs one connect call in the background and returns a wait that
// can be called more than once, so a cleanup can join it after the test did.
func joinOnce(run func() error) func() error {
	done := make(chan error, 1)
	go func() { done <- run() }()
	var (
		once sync.Once
		err  error
	)
	return func() error {
		once.Do(func() { err = <-done })
		return err
	}
}

func TestCreateRoom_SecondConnectWhileInFlightOpensNoSecondBridgeLeg(t *testing.T) {
	// Long rounds, so the create returns on the cancel below and not on the
	// round's own clock.
	origWait := bridgeRoundWait
	bridgeRoundWait = 10 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, _ := rendezvousPair(t)
	bridge := newTokenBridge(t, creator)
	kd, _ := rendezvousKD(t, creator, bridge.addr)
	kd.RelayEndoint = registerOnlyRelay(t)
	kd.relayClient = http.DefaultClient
	kd.markInboundBlocked() // the field shape: bridge only, as both peers were on 2026-09-16

	// The auto-connect dial: a create that reaches the bridge and waits there.
	waitFirst := joinOnce(kd.CreateRoom)
	t.Cleanup(func() {
		kd.NotifyDisconnect()
		_ = waitFirst()
	})

	leg, err := bridge.take(bridge.pair1, "pair1")
	require.NoError(t, err, "the first create reaches the bridge")
	defer leg.Close()

	// The click. On 0.4.8 this ran a second create to completion.
	secondErr := make(chan error, 1)
	go func() { secondErr <- kd.Connect() }()
	select {
	case err := <-secondErr:
		require.ErrorIs(t, err, ErrConnectInProgress)
	case <-time.After(3 * time.Second):
		t.Fatal("the second Connect did not return: it is running a create of its own")
	}
	// The Create Room and Join Room buttons, and mobile's async ops, call these directly.
	require.ErrorIs(t, kd.CreateRoom(), ErrConnectInProgress)
	require.ErrorIs(t, kd.JoinRoom(), ErrConnectInProgress)

	// The bridge holds one pair1 leg for this room, the first create's.
	select {
	case c := <-bridge.pair1:
		_ = c.Close()
		t.Fatal("a second pair1 leg reached the bridge: the room is paired with itself")
	case <-time.After(500 * time.Millisecond):
	}
	require.True(t, kd.connectInFlight.Load(), "the refused calls must not release the first create's slot")

	// Cancel ends the create now, not at the end of its 10 s round, and the
	// retry that follows a Cancel is admitted while the create unwinds.
	kd.NotifyDisconnect()
	retry := make(chan error, 1)
	go func() {
		release, err := kd.beginConnect()
		if err == nil {
			release()
		}
		retry <- err
	}()
	start := time.Now()
	err = waitFirst()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cancelled")
	require.Less(t, time.Since(start), 2*time.Second, "the cancel must end the round at once")
	select {
	case err := <-retry:
		require.NoError(t, err, "a retry during the unwind takes the slot once it is free")
	case <-time.After(3 * time.Second):
		t.Fatal("the retry never got the slot")
	}
	require.False(t, kd.connectInFlight.Load(), "the slot is released when the create returns")
}

// A joiner whose peer is not on the relay yet polls the relay for a minute.
// Cancel has to end that wait, or the slot stays taken for the whole minute
// and every retry in it is refused.
func TestJoinRoom_CancelEndsTheRelayWait(t *testing.T) {
	_, joiner := rendezvousPair(t)
	kd, _ := rendezvousKD(t, joiner, silentBridge(t))
	kd.RelayEndoint = registerOnlyRelay(t) // fetch answers 404: the peer is not there
	kd.relayClient = http.DefaultClient

	waitJoin := joinOnce(kd.JoinRoom)
	t.Cleanup(func() {
		kd.NotifyDisconnect()
		_ = waitJoin()
	})

	// Let the join reach its relay wait, then cancel.
	time.Sleep(300 * time.Millisecond)
	require.True(t, kd.connectInFlight.Load(), "the join is in flight")
	start := time.Now()
	kd.NotifyDisconnect()
	err := waitJoin()
	require.ErrorIs(t, err, ErrConnectCancelled)
	require.Less(t, time.Since(start), 2*time.Second, "the cancel must end the relay wait at once")
	require.False(t, kd.connectInFlight.Load())
}

// The joiner's inbound bridge leg waits joinBridgeWait, a minute, for the
// creator's handshake. Cancel closes the leg under the wait.
func TestJoinBridgeInbound_CancelEndsTheWait(t *testing.T) {
	_, joiner := rendezvousPair(t)
	kd := newBareKD()
	kd.session = joiner

	// A bridge that pairs nobody: the leg stays silent for the whole wait.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	leg, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)

	release, err := kd.beginConnect()
	require.NoError(t, err)
	defer release()

	waitLeg := joinOnce(func() error { return kd.joinBridgeInbound(leg) })
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	kd.NotifyDisconnect()
	err = waitLeg()
	require.ErrorIs(t, err, ErrConnectCancelled)
	require.Less(t, time.Since(start), 2*time.Second, "the cancel must close the leg at once")
}

// The frontends run their process-lifetime loops (presence heartbeat,
// auto-connect watchdog, throughput sampler, reachability probe) on the
// context they build the engine from. A disconnect cancels the engine's own
// session context and must leave that one alive. The desktop app broke this
// from outside on 0.4.8 by storing its process cancel in kd.Cancel, which
// Stop calls; this pins the engine's half of the contract.
func TestSessionCancel_LeavesTheFrontendContextAlive(t *testing.T) {
	parent := t.Context()
	relay, err := url.Parse("http://127.0.0.1:54321")
	require.NoError(t, err)
	kd, err := NewKeibiDrop(parent, testkit.DiscardLogger(), false, relay, 0, 0, t.TempDir(), t.TempDir(), false, false)
	require.NoError(t, err)
	t.Cleanup(kd.Shutdown)

	kd.cancelContext() // what Stop does once a session runs
	require.NoError(t, parent.Err(), "the engine's session cancel must not end the frontend's context")
	require.Error(t, kd.ctx.Err(), "the session context itself is cancelled")
}
