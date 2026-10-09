// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.
// A connect while the daemon still runs a session with no link (its reconnect
// retrying, waiting for the peer, or given up) ends that session and runs,
// instead of answering "already running" until the retry budget ends. Seen
// 2026-09-28 by the web chaos fuzzer: a tab reloaded mid-reconnect could not
// pair again without a kd disconnect first (KeibiDropWeb issue 67).

package common

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// countingRelay registers rooms and never finds a peer. It counts requests,
// so a test can tell that a connect got past the running guard.
func countingRelay(t *testing.T) (*url.URL, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path == "/register" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u, &hits
}

// linklessKD is an engine with its Run loop, marked running with peerFP as
// its session's peer, and a reconnect manager in the given state: the shape
// of a session whose link dropped.
func linklessKD(t *testing.T, relay *url.URL, peerFP string, state session.ReconnectState) *KeibiDrop {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := pickFreePortPair(t) // The engine refuses a listen port outside the peer range.
	kd, err := NewKeibiDropWithIP(ctx, roundTestLogger(), false, relay, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err)
	kd.BridgeAddr = "127.0.0.1:9"
	go kd.Run()
	t.Cleanup(kd.Shutdown)

	kd.session.ExpectedPeerFingerprint = peerFP
	rm := session.NewReconnectManager(kd.session, kd.logger)
	switch state {
	case session.ReconnectStateGaveUp:
		rm.MaxAttempts = 0
		rm.OnDisconnect()
		require.Eventually(t, func() bool { return rm.State() == session.ReconnectStateGaveUp },
			5*time.Second, 10*time.Millisecond)
	case session.ReconnectStateReconnecting:
		rm.Backoff = []time.Duration{time.Hour} // parks on the first backoff
		rm.OnDisconnect()
	}
	require.Equal(t, state, rm.State())
	kd.mu.Lock()
	kd.ReconnectManager = rm
	kd.mu.Unlock()
	kd.running.Store(true)
	return kd
}

func TestConnect_SupersedesASessionWithNoLink(t *testing.T) {
	for _, state := range []session.ReconnectState{session.ReconnectStateReconnecting, session.ReconnectStateGaveUp} {
		t.Run(state.String(), func(t *testing.T) {
			relay, hits := countingRelay(t)
			peer, _ := rendezvousPair(t)
			kd := linklessKD(t, relay, peer.OwnFingerprint, state)
			// The reloaded tab keeps its identity: kd register writes the same peer.
			require.NoError(t, kd.AddPeerFingerprint(peer.OwnFingerprint))

			done := make(chan error, 1)
			go func() { done <- kd.Connect() }()

			require.Eventually(t, func() bool { return !kd.running.Load() },
				10*time.Second, 20*time.Millisecond, "the session with no link ends")
			require.Eventually(t, func() bool { return hits.Load() > 0 },
				10*time.Second, 20*time.Millisecond, "the fresh connect reaches the relay")
			kd.mu.Lock()
			got := kd.session.ExpectedPeerFingerprint
			kd.mu.Unlock()
			require.Equal(t, peer.OwnFingerprint, got, "the registered peer carries over to the fresh session")

			kd.CancelPendingConnect()
			select {
			case err := <-done:
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrAlreadyRunning)
			case <-time.After(30 * time.Second):
				t.Fatal("the connect did not end after the cancel")
			}
		})
	}
}

// A session with a live link is not superseded: the caller is told it is
// running, as before.
func TestConnect_RefusesWhileTheSessionHasALink(t *testing.T) {
	relay, hits := countingRelay(t)
	peer, _ := rendezvousPair(t)
	kd := linklessKD(t, relay, peer.OwnFingerprint, session.ReconnectStateConnected)
	require.NoError(t, kd.AddPeerFingerprint(peer.OwnFingerprint))

	require.ErrorIs(t, kd.Connect(), ErrAlreadyRunning)
	require.True(t, kd.running.Load())
	require.Zero(t, hits.Load())
}

// The auto-connect watchdog never supersedes: it defers to the reconnect, and
// a session that gave up ends through OnGaveUp when a contact is armed.
func TestConnect_WatchdogDoesNotSupersede(t *testing.T) {
	relay, hits := countingRelay(t)
	peer, _ := rendezvousPair(t)
	kd := linklessKD(t, relay, peer.OwnFingerprint, session.ReconnectStateGaveUp)
	require.NoError(t, kd.AddPeerFingerprint(peer.OwnFingerprint))

	require.ErrorIs(t, kd.connect(originWatchdog), ErrAlreadyRunning)
	require.True(t, kd.running.Load())
	require.Zero(t, hits.Load())
}
