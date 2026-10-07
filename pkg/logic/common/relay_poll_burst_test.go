// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.
// When both sides connect at once, the joiner's first relay fetch lands before
// the creator has registered. A one-second sleep before the second fetch was
// the whole connect time of a bridge pair: about 1 s against 70 ms once the
// registration is seen (loopback through the production relay and bridge,
// 2026-09-30; KeibiDropWeb issue 78). The first polls are quick.

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
)

func TestJoinRoom_FirstRelayPollsAreQuick(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			polls.Add(1)
		}
		http.NotFound(w, r) // the creator has not registered yet
	}))
	t.Cleanup(srv.Close)
	relay, err := url.Parse(srv.URL)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := freeTCPPort(t)
	kd, err := NewKeibiDropWithIP(ctx, roundTestLogger(), false, relay, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err)
	kd.BridgeAddr = "127.0.0.1:9"
	peer, _ := rendezvousPair(t)
	require.NoError(t, kd.AddPeerFingerprint(peer.OwnFingerprint))

	done := make(chan error, 1)
	go func() { done <- kd.JoinRoom() }()
	time.Sleep(1500 * time.Millisecond)
	kd.CancelPendingConnect()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the join did not end after the cancel")
	}
	require.GreaterOrEqual(t, polls.Load(), int32(5), "five polls in the first second, not one per second")
}
