//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"testing"
	"time"
)

// TestManagerControlPlane brings up the QUIC control plane and checks the heartbeat
// measured an RTT, the loopback link reads clean, and bulk routes to TCP while QUIC
// bulk is not viable.
func TestManagerControlPlane(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	srv, addr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m, err := DialControl(ctx, addr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()

	if rtt := m.RTT(); rtt <= 0 {
		t.Fatalf("heartbeat did not measure an RTT: %v", rtt)
	}
	if q := m.LinkQuality(); q != QualityClean {
		t.Fatalf("loopback should read clean, got %s (rtt %v)", q, m.RTT())
	}
	if _, ok := m.BulkTransport().(tcpTransport); !ok {
		t.Fatal("bulk should route to TCP while QUIC bulk is not viable (sendmsg_x pending)")
	}
}

// TestManagerMigration migrates the control plane to a fresh local socket, asserts the
// network-change event fires with a changed address, and asserts the control plane still
// answers afterward (which a TCP control channel could not do across an address change).
func TestManagerMigration(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	srv, addr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		t.Fatalf("server: %v", err)
	}
	defer srv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := DialControl(ctx, addr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()

	evCh := make(chan NetworkEvent, 1)
	m.OnNetworkChange(func(e NetworkEvent) { evCh <- e })

	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	select {
	case e := <-evCh:
		if e.OldLocalAddr == nil || e.NewLocalAddr == nil {
			t.Fatal("network-change event missing addresses")
		}
		if e.OldLocalAddr.String() == e.NewLocalAddr.String() {
			t.Fatalf("migration did not change the local address: %v", e.NewLocalAddr)
		}
		t.Logf("network changed: %v -> %v", e.OldLocalAddr, e.NewLocalAddr)
	case <-time.After(3 * time.Second):
		t.Fatal("no network-change event fired")
	}

	// The session survived the address change: the control plane still answers.
	if err := m.pingOnce(ctx); err != nil {
		t.Fatalf("control plane broken after migration: %v", err)
	}
}

// TestBulkRouting pins the routing rule in both capability states: while QUIC bulk is
// not viable every link routes bulk to TCP; once viable, only a clean link flips to QUIC.
func TestBulkRouting(t *testing.T) {
	// Current reality: QUIC bulk not viable -> TCP everywhere.
	for _, q := range []Quality{QualityClean, QualityDegraded, QualityUnknown, QualityDead} {
		if _, ok := bulkTransportFor(q).(tcpTransport); !ok {
			t.Fatalf("%s: bulk should be TCP while QUIC bulk is not viable", q)
		}
	}

	// Future: sendmsg_x lands -> clean link routes bulk to QUIC.
	quicBulkViable = true
	defer func() { quicBulkViable = false }()
	cases := []struct {
		q        Quality
		wantQUIC bool
	}{
		{QualityClean, true},
		{QualityDegraded, false},
		{QualityUnknown, false},
		{QualityDead, false},
	}
	for _, c := range cases {
		_, isQUIC := bulkTransportFor(c.q).(quicTransport)
		if isQUIC != c.wantQUIC {
			t.Fatalf("viable %s: got quic=%v want quic=%v", c.q, isQUIC, c.wantQUIC)
		}
	}
}
