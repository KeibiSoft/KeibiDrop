// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// A second CreateRoom, JoinRoom or Connect while one is in flight is refused
// and opens nothing. Seen 2026-09-16 on 0.4.8: auto-connect had the creator's
// CreateRoom waiting on its bridge leg, a click on Connect reached CreateRoom
// again, the second leg carried the same pair1 token, the bridge paired the
// creator with itself and the real joiner read EOF. Every frontend guards its
// own button and none of them sees the engine's auto-connect, so the guard
// lives in the engine and this file drives the real CreateRoom through it.

package common

import (
	"io"
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
// a 404, which leaves the blocked mark as the test set it.
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

func TestCreateRoom_SecondConnectWhileInFlightOpensNoSecondBridgeLeg(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 2 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, _ := rendezvousPair(t)
	bridge := newTokenBridge(t, creator)
	kd, _ := rendezvousKD(t, creator, bridge.addr)
	kd.RelayEndoint = registerOnlyRelay(t)
	kd.relayClient = http.DefaultClient
	kd.markInboundBlocked() // the field shape: bridge only, as both peers were on 2026-09-16

	// The auto-connect dial: a create that reaches the bridge and waits there.
	firstDone := make(chan error, 1)
	go func() { firstDone <- kd.CreateRoom() }()
	var (
		firstOnce sync.Once
		firstErr  error
	)
	waitFirst := func() error {
		firstOnce.Do(func() { firstErr = <-firstDone })
		return firstErr
	}
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

	// The first create is the one that ends the slot: aborted, it returns and
	// the next connect is admitted.
	kd.NotifyDisconnect()
	err = waitFirst()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cancelled")
	require.False(t, kd.connectInFlight.Load(), "the slot is released when the create returns")
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
