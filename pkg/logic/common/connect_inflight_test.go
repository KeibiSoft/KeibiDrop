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
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
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

	// The click, for the same peer. On 0.4.8 this ran a second create to
	// completion. Now it joins the one in flight: it opens nothing of its own
	// and returns with that create's result. The Create Room and Join Room
	// buttons, and mobile's async ops, call the two rooms directly.
	joinedErr := make(chan error, 3)
	go func() { joinedErr <- kd.Connect() }()
	go func() { joinedErr <- kd.CreateRoom() }()
	go func() { joinedErr <- kd.JoinRoom() }()
	select {
	case err := <-joinedErr:
		t.Fatalf("a same-peer call returned on its own instead of joining: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	// The bridge holds one pair1 leg for this room, the first create's.
	select {
	case c := <-bridge.pair1:
		_ = c.Close()
		t.Fatal("a second pair1 leg reached the bridge: the room is paired with itself")
	case <-time.After(500 * time.Millisecond):
	}
	require.True(t, kd.connectInFlight.Load(), "the joined calls must not release the first create's slot")

	// Cancel ends the create now, not at the end of its 10 s round; the joined
	// calls get the same result; and the retry that follows a Cancel is
	// admitted while the create unwinds.
	kd.NotifyDisconnect()
	retry := make(chan error, 1)
	go func() {
		p, joined, err := kd.beginConnect(originUser)
		if err == nil && !joined {
			kd.endConnect(p, nil)
		}
		retry <- err
	}()
	start := time.Now()
	err = waitFirst()
	require.ErrorIs(t, err, ErrConnectCancelled)
	require.Less(t, time.Since(start), 2*time.Second, "the cancel must end the round at once")
	for range 3 {
		select {
		case err := <-joinedErr:
			require.ErrorIs(t, err, ErrConnectCancelled, "a joined call carries the create's result")
		case <-time.After(3 * time.Second):
			t.Fatal("a joined call never returned")
		}
	}
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

	p, joined, err := kd.beginConnect(originUser)
	require.NoError(t, err)
	require.False(t, joined)
	defer kd.endConnect(p, nil)

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

// The 0.4.8 desktop app stored its process cancel in kd.Cancel. The engine
// cannot stop that from outside (Cancel is the seam tests and the run loop
// use), so it names it: a disconnect that ends the frontend's context logs
// an error instead of going silent.
func TestSessionCancel_ReportsAReplacedCancelThatEndsTheFrontendContext(t *testing.T) {
	var logs bytes.Buffer
	parent, parentCancel := context.WithCancel(t.Context())
	defer parentCancel()
	relay, err := url.Parse("http://127.0.0.1:54321")
	require.NoError(t, err)
	kd, err := NewKeibiDrop(parent, testkit.Logger(&logs, slog.LevelDebug), false, relay, 0, 0, t.TempDir(), t.TempDir(), false, false)
	require.NoError(t, err)
	t.Cleanup(kd.Shutdown)

	kd.Cancel = parentCancel // the desktop app's mistake
	kd.cancelContext()       // what Stop does once a session runs
	require.Error(t, parent.Err())
	require.Contains(t, logs.String(), "ended the frontend's process context")

	// At app exit the frontend ends its own context; that is not the mistake.
	logs.Reset()
	kd.Shutdown()
	require.NotContains(t, logs.String(), "ended the frontend's process context")
}

// otherPeerFingerprint is a valid fingerprint of a third party, for the
// "another peer was asked for" cases.
func otherPeerFingerprint(t *testing.T) string {
	t.Helper()
	s, err := session.InitSession(testkit.DiscardLogger(), 26046, 26045)
	require.NoError(t, err)
	fp, err := s.OwnKeys.Fingerprint()
	require.NoError(t, err)
	return fp
}

// A person who pastes another code while a connect is in flight has chosen:
// the running connect is displaced at once, and the session now names the
// new peer. Left alone, the old create's later rounds hashed the new peer
// into their bridge token and the old peer never paired.
func TestAddPeerFingerprint_AnotherPeerDisplacesTheConnectInFlight(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 10 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, _ := rendezvousPair(t)
	bridge := newTokenBridge(t, creator)
	kd, _ := rendezvousKD(t, creator, bridge.addr)
	kd.RelayEndoint = registerOnlyRelay(t)
	kd.relayClient = http.DefaultClient
	kd.markInboundBlocked()

	waitFirst := joinOnce(kd.CreateRoom)
	t.Cleanup(func() {
		kd.NotifyDisconnect()
		_ = waitFirst()
	})
	leg, err := bridge.take(bridge.pair1, "pair1")
	require.NoError(t, err)
	defer leg.Close()

	other := otherPeerFingerprint(t)
	start := time.Now()
	require.NoError(t, kd.AddPeerFingerprint(other))
	err = waitFirst()
	require.ErrorIs(t, err, ErrConnectCancelled)
	require.Less(t, time.Since(start), 2*time.Second, "the new peer must end the old create at once")
	require.Equal(t, other, kd.session.ExpectedPeerFingerprint)
	require.False(t, kd.connectInFlight.Load())
}

// The watchdog never displaces a person: its dial for its own contact is
// refused while a connect to someone else runs, and that connect is untouched.
func TestWatchdogDial_NeverDisplacesAPersonsConnect(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 10 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, _ := rendezvousPair(t)
	bridge := newTokenBridge(t, creator)
	kd, _ := rendezvousKD(t, creator, bridge.addr)
	kd.RelayEndoint = registerOnlyRelay(t)
	kd.relayClient = http.DefaultClient
	kd.markInboundBlocked()
	person := creator.ExpectedPeerFingerprint

	waitFirst := joinOnce(kd.CreateRoom)
	t.Cleanup(func() {
		kd.NotifyDisconnect()
		_ = waitFirst()
	})
	leg, err := bridge.take(bridge.pair1, "pair1")
	require.NoError(t, err)
	defer leg.Close()

	other := otherPeerFingerprint(t)
	ab, err := identity.LoadAddressBook(t.TempDir(), nil)
	require.NoError(t, err)
	require.NoError(t, ab.Add("box", other))
	kd.AddressBook = ab

	require.ErrorIs(t, kd.watchdogDial(other), ErrConnectInProgress)
	require.True(t, kd.connectInFlight.Load(), "the person's connect keeps its slot")
	require.False(t, kd.connectAbortRequested(), "and was not cancelled")
	require.Equal(t, person, kd.session.ExpectedPeerFingerprint, "and keeps its peer")
}
