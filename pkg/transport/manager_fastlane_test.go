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
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
	"github.com/stretchr/testify/require"
)

// twoChannelServers starts the QUIC server (control + interactive) and the TCP server
// (bulk) for one peer identity, mirroring a real peer on both a UDP and TCP endpoint.
func twoChannelServers(t *testing.T, serverID *Identity, clientFP string) (quicAddr, tcpAddr string, stop func()) {
	t.Helper()
	qsrv, qa, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientFP)
	require.NoError(t, err, "quic server")
	tsrv, ta, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{}, serverID, clientFP)
	if err != nil {
		qsrv.Stop()
		require.NoError(t, err, "tcp server")
	}
	return qa.String(), ta.String(), func() { qsrv.Stop(); tsrv.Stop() }
}

// TestInteractiveFastLane: with bulk saturating the TCP channel, a seek on the QUIC
// channel is isolated by transport, while the same seek muxed onto the busy TCP channel
// head-of-line blocks. On loopback the gap is small (no bottleneck to congest), so this
// reports both latencies and asserts correctness under load.
func TestInteractiveFastLane(t *testing.T) {
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

		bulk := pb.NewBenchServiceClient(m.BulkConn())
		inter := pb.NewBenchServiceClient(m.InteractiveConn())

		// Saturate the TCP bulk channel in the background.
		stopBulk := saturateBulk(ctx, bulk, 64<<20)
		defer stopBulk()

		time.Sleep(200 * time.Millisecond) // let the saturator fill the pipe

		measure := func(echo func(context.Context) error) ([]time.Duration, error) {
			const n = 40
			lat := make([]time.Duration, 0, n)
			for i := 0; i < n; i++ {
				c, cancel := context.WithTimeout(ctx, 5*time.Second)
				t0 := time.Now()
				err := echo(c)
				d := time.Since(t0)
				cancel()
				if err != nil {
					return nil, fmt.Errorf("urgent echo %d: %w", i, err)
				}
				lat = append(lat, d)
			}
			return lat, nil
		}

		// Seek muxed onto the busy TCP bulk channel (head-of-line blocking).
		muxed, err := measure(func(c context.Context) error {
			_, err := bulk.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
			return err
		})
		if err != nil {
			return err
		}
		// Seek on the QUIC channel: isolated from TCP bulk by transport (the fast lane).
		fast, err := measure(func(c context.Context) error {
			_, err := inter.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
			return err
		})
		if err != nil {
			return err
		}

		t.Logf("seek under saturating bulk (loopback):")
		t.Logf("  muxed on TCP bulk (HoL): p50=%v p99=%v max=%v", pctile(muxed, .5), pctile(muxed, .99), pctile(muxed, 1))
		t.Logf("  QUIC fast lane         : p50=%v p99=%v max=%v", pctile(fast, .5), pctile(fast, .99), pctile(fast, 1))
		if pctile(fast, .5) > pctile(muxed, .5) {
			t.Logf("  note: fast-lane p50 not lower on loopback (no bottleneck to congest; WAN is where it shows)")
		}
		return nil
	})
}

// TestManagerBulkReconstructOnMigrate: after the QUIC control plane migrates, the TCP
// bulk channel (whose 5-tuple died) is reconstructed, works again, and is a different
// connection than before.
func TestManagerBulkReconstructOnMigrate(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	quicAddr, tcpAddr, stop := twoChannelServers(t, serverID, clientID.Fingerprint())
	defer stop()

	testkit.Run(t, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m := testkit.Must(DialControl(ctx, quicAddr, clientID, serverID.Fingerprint()))
		defer m.Close()
		if err := m.DialBulk(ctx, tcpAddr); err != nil {
			return err
		}

		before := m.BulkConn()
		if _, err := pb.NewBenchServiceClient(before).Echo(ctx, &pb.EchoRequest{Payload: []byte("pre")}); err != nil {
			return fmt.Errorf("bulk echo before migration: %w", err)
		}

		if err := m.Migrate(ctx); err != nil {
			return err
		}

		after := m.BulkConn()
		if err := fp.All(
			fp.True("bulk channel reconstructed after migration", after != nil),
			fp.NotEqual("bulk conn after migration", after, before),
		); err != nil {
			return err
		}
		_, err := pb.NewBenchServiceClient(after).Echo(ctx, &pb.EchoRequest{Payload: []byte("post")})
		return fp.NoErr("bulk echo after reconstruction", err)
	})
}

// TestBulkResumeAcrossReconstruct proves resume-by-offset: a bulk transfer interrupted
// mid-flight by a channel reconstruction resumes from the last received offset. Asserts
// completion, byte-correctness, contiguous offsets, and that the server saw StartOffset
// > 0 (a real resume, not a silent restart).
func TestBulkResumeAcrossReconstruct(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()

	testkit.Run(t, func() error {
		qsrv, qaddr := testkit.Must2(ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint()))
		defer qsrv.Stop()
		var maxStart atomic.Uint64
		tsrv, taddr, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{maxStart: &maxStart}, serverID, clientID.Fingerprint())
		if err != nil {
			qsrv.Stop()
			return err
		}
		defer tsrv.Stop()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		m := testkit.Must(DialControl(ctx, qaddr.String(), clientID, serverID.Fingerprint()))
		defer m.Close()
		if err := m.DialBulk(ctx, taddr.String()); err != nil {
			return err
		}

		const total = 32 << 20
		const chunk = 64 << 10
		var received uint64
		interrupted := false
		err = m.BulkGet(ctx, total, chunk, func(off uint64, data []byte) {
			if !verifyChunk(data) {
				t.Errorf("corrupt chunk at offset %d", off)
			}
			if off != received {
				t.Errorf("offset gap: chunk offset=%d but received=%d", off, received)
			}
			received += uint64(len(data))
			if !interrupted && received >= total/2 {
				interrupted = true
				m.reconstructBulk(ctx) // simulate a network-change reconstruction mid-transfer
			}
		})
		if err != nil {
			return err
		}
		if err := fp.All(
			fp.Equal("bytes received", received, uint64(total)),
			fp.True("resume happened: server saw StartOffset > 0", maxStart.Load() != 0),
		); err != nil {
			return err
		}
		t.Logf("interrupted mid-transfer, resumed from offset %d of %d, completed byte-correct", maxStart.Load(), uint64(total))
		return nil
	})
}
