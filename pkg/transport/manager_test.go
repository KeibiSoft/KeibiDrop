//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// TestManagerControlPlane brings up the QUIC control plane and checks the heartbeat
// measured an RTT, the loopback link reads clean, and bulk routes to TCP while QUIC
// bulk is not viable.
func TestManagerControlPlane(t *testing.T) {
	testkit.Run(t, func() error {
		serverID, _ := NewIdentity()
		clientID, _ := NewIdentity()
		srv, addr := testkit.Must2(ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint()))
		defer srv.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		m := testkit.Must(DialControl(ctx, addr.String(), clientID, serverID.Fingerprint()))
		defer m.Close()

		_, bulkIsTCP := m.BulkTransport().(tcpTransport)
		return fp.All(
			fp.True("heartbeat measured a positive RTT", m.RTT() > 0),
			fp.Equal("link quality", m.LinkQuality(), QualityClean),
			fp.True("bulk routes to TCP while QUIC bulk is not viable (sendmsg_x pending)", bulkIsTCP),
		)
	})
}

// TestManagerMigration migrates the control plane to a fresh local socket, asserts the
// network-change event fires with a changed address, and asserts the control plane still
// answers afterward (which a TCP control channel could not do across an address change).
func TestManagerMigration(t *testing.T) {
	testkit.Run(t, func() error {
		serverID, _ := NewIdentity()
		clientID, _ := NewIdentity()
		srv, addr := testkit.Must2(ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint()))
		defer srv.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m := testkit.Must(DialControl(ctx, addr.String(), clientID, serverID.Fingerprint()))
		defer m.Close()

		evCh := make(chan NetworkEvent, 1)
		m.OnNetworkChange(func(e NetworkEvent) { evCh <- e })

		if err := m.Migrate(ctx); err != nil {
			return err
		}

		select {
		case e := <-evCh:
			if err := fp.Steps(
				func() error { return fp.True("network-change event has addresses", e.OldLocalAddr != nil && e.NewLocalAddr != nil) },
				func() error {
					return fp.NotEqual("local address after migration", e.NewLocalAddr.String(), e.OldLocalAddr.String())
				},
			); err != nil {
				return err
			}
			t.Logf("network changed: %v -> %v", e.OldLocalAddr, e.NewLocalAddr)
		case <-time.After(3 * time.Second):
			return errors.New("no network-change event fired")
		}

		// The session survived the address change: the control plane still answers.
		return m.pingOnce(ctx)
	})
}

// TestBulkRouting pins the routing rule in both capability states: while QUIC bulk is
// not viable every link routes bulk to TCP; once viable, only a clean link flips to QUIC.
func TestBulkRouting(t *testing.T) {
	testkit.Run(t, func() error {
		// Current reality: QUIC bulk not viable -> TCP everywhere.
		qualities := []Quality{QualityClean, QualityDegraded, QualityUnknown, QualityDead}
		if err := fp.Each("bulk transport while QUIC bulk not viable", qualities, func(q Quality) error {
			_, ok := bulkTransportFor(q).(tcpTransport)
			return fp.True("routes bulk to TCP", ok)
		}); err != nil {
			return err
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
			if err := fp.Equal(fmt.Sprintf("viable %s", c.q), isQUIC, c.wantQUIC); err != nil {
				return err
			}
		}
		return nil
	})
}
