//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestMissReadUnderBulk: while a bulk transfer saturates the TCP channel, a 512 KiB
// range read (a FUSE miss for KeibiDrop's block size) over the QUIC interactive channel
// returns fast and byte-correct instead of queuing behind the bulk transfer.
func TestMissReadUnderBulk(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	quicAddr, tcpAddr, stop := twoChannelServers(t, serverID, clientID.Fingerprint())
	defer stop()

	testkit.Run(t, func() error {
		ctx := context.Background()
		m := testkit.Must(DialControl(ctx, quicAddr, clientID, serverID.Fingerprint()))
		defer m.Close()
		if err := m.DialBulk(ctx, tcpAddr); err != nil {
			return err
		}

		// Saturate the TCP bulk channel in the background.
		bulk := pb.NewBenchServiceClient(m.BulkConn())
		stopBulk := saturateBulk(ctx, bulk, 64<<20)
		defer stopBulk()
		time.Sleep(200 * time.Millisecond)

		// Cache-miss reads over the QUIC interactive channel, under the bulk load.
		const n = 40
		const readLen = 512 << 10 // KeibiDrop's block: the first chunk fetched on a miss
		lat := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			off := uint64(i) * readLen
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			t0 := time.Now()
			data, err := m.MissRead(c, off, readLen)
			d := time.Since(t0)
			cancel()
			if err != nil {
				return fmt.Errorf("miss read %d: %w", i, err)
			}
			if len(data) != readLen || !verifyRange(data, off) {
				return fmt.Errorf("miss read %d: wrong bytes (len %d)", i, len(data))
			}
			lat = append(lat, d)
		}

		t.Logf("512 KiB cache-miss read over QUIC, under saturating TCP bulk (loopback):")
		t.Logf("  p50=%v  p99=%v  max=%v  (byte-correct)", pctile(lat, .5), pctile(lat, .99), pctile(lat, 1))
		return nil
	})
}
