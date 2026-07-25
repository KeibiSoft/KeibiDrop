// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests the inbound reachability mark and the relay probe that sets it.

package common

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func probeKD(t *testing.T, endpoint string) *KeibiDrop {
	t.Helper()
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	return &KeibiDrop{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		relayClient:  http.DefaultClient,
		RelayEndoint: u,
		inboundPort:  26001,
		ctx:          context.Background(),
		LocalIPv6IP:  "2001:db8::1",
	}
}

// The mark must not outlive the network it was observed on: one blocked hotel Wi-Fi must not
// pin a peer to the relay forever once it moves.
func TestReachability_MarkIsScopedToTheLocalAddress(t *testing.T) {
	kd := probeKD(t, "http://relay.invalid")

	kd.markInboundBlocked()
	require.True(t, kd.InboundBlocked(), "the mark holds on the address it was observed on")

	kd.LocalIPv6IP = "2001:db8::2" // moved networks
	require.False(t, kd.InboundBlocked(), "a different address gets a fresh verdict")

	kd.LocalIPv6IP = "2001:db8::1" // and the stale verdict is not resurrected
	require.False(t, kd.InboundBlocked())
}

// An inbound connection that actually arrives clears the mark.
func TestReachability_ReachableClearsTheMark(t *testing.T) {
	kd := probeKD(t, "http://relay.invalid")
	kd.markInboundBlocked()
	require.True(t, kd.InboundBlocked())

	kd.markInboundReachable()
	require.False(t, kd.InboundBlocked())
}

// The probe's verdict drives the mark, and it asks for the port we actually listen on.
func TestReachability_ProbeSetsTheMark(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantBlocked bool
	}{
		{"relay reached us", `{"reachable":true}`, false},
		{"relay could not reach us", `{"reachable":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPort string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPort = r.URL.Query().Get("port")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			kd := probeKD(t, srv.URL)
			kd.ProbeInboundReachability(context.Background())

			require.Equal(t, "26001", gotPort, "the probe asks about our own listener port")
			require.Equal(t, tc.wantBlocked, kd.InboundBlocked())
		})
	}
}

// A relay without the endpoint must cost nothing: the mark stays as observed, so the slow
// observational path still works against an older relay.
func TestReachability_ProbeIsBestEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	kd := probeKD(t, srv.URL)
	kd.markInboundBlocked()
	kd.ProbeInboundReachability(context.Background())
	require.True(t, kd.InboundBlocked(), "a 404 leaves the observed verdict untouched")

	kd.markInboundReachable()
	kd.ProbeInboundReachability(context.Background())
	require.False(t, kd.InboundBlocked())
}

// The keepalive re-registers constantly, so the probe must run once per address, not per call.
func TestReachability_ProbeRunsOncePerAddress(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"reachable":true}`)
	}))
	defer srv.Close()

	kd := probeKD(t, srv.URL)
	for i := 0; i < 5; i++ {
		kd.ProbeInboundReachability(context.Background())
	}
	require.Equal(t, int32(1), calls.Load(), "re-registration must not re-probe")

	kd.LocalIPv6IP = "2001:db8::2" // a new network is worth asking about again
	kd.ProbeInboundReachability(context.Background())
	require.Equal(t, int32(2), calls.Load())
}

// Swapping the router, or changing its firewall, changes whether we are reachable while the ISP
// re-delegates the same prefix and our address never moves. A stale "blocked" cannot correct
// itself (we tell the peer not to dial and skip our own accept, so no inbound can disprove it),
// so the verdict has to expire and be re-probed.
func TestReachability_VerdictExpiresSoAFixedRouterIsPickedUp(t *testing.T) {
	var reachable atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reachable.Load() {
			_, _ = io.WriteString(w, `{"reachable":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"reachable":false}`)
	}))
	defer srv.Close()

	kd := probeKD(t, srv.URL)
	kd.ProbeInboundReachability(context.Background())
	require.True(t, kd.InboundBlocked(), "the router is dropping inbound")

	// The router is fixed. Nothing about our address changed, and no inbound can arrive to
	// disprove the mark, so only expiry gets us back onto the direct path.
	reachable.Store(true)
	kd.ProbeInboundReachability(context.Background())
	require.True(t, kd.InboundBlocked(), "within the TTL the cached verdict still holds")

	kd.probedAt.Store(time.Now().Add(-probeCacheTTL - time.Minute).UnixNano())
	kd.ProbeInboundReachability(context.Background())
	require.False(t, kd.InboundBlocked(), "an expired verdict is re-probed and cleared")
}

// The hint is published for the peer and read back from its registration; an older peer that
// sends no field decodes as false, so the direct path is tried exactly as before.
func TestReachability_HintTravelsInTheRegistration(t *testing.T) {
	kd := probeKD(t, "http://relay.invalid")
	require.False(t, kd.PeerInboundBlocked(), "absent field means try direct")

	kd.peerInboundBlocked.Store(true)
	require.True(t, kd.PeerInboundBlocked())
}
