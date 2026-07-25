//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// twoChannelServers starts the QUIC server (control + interactive) and the TCP server
// (bulk) for one peer identity, mirroring a real peer on both a UDP and TCP endpoint.
func twoChannelServers(t *testing.T, serverID *Identity, clientFP string) (quicAddr, tcpAddr string, stop func()) {
	t.Helper()
	qsrv, qa, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientFP)
	if err != nil {
		t.Fatalf("quic server: %v", err)
	}
	tsrv, ta, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{}, serverID, clientFP)
	if err != nil {
		qsrv.Stop()
		t.Fatalf("tcp server: %v", err)
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

	ctx := context.Background()
	m, err := DialControl(ctx, quicAddr, clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, tcpAddr); err != nil {
		t.Fatalf("DialBulk: %v", err)
	}

	bulk := pb.NewBenchServiceClient(m.BulkConn())
	inter := pb.NewBenchServiceClient(m.InteractiveConn())

	// Saturate the TCP bulk channel in the background.
	stopBulk := saturateBulk(ctx, bulk, 64<<20)
	defer stopBulk()

	time.Sleep(200 * time.Millisecond) // let the saturator fill the pipe

	measure := func(echo func(context.Context) error) []time.Duration {
		const n = 40
		lat := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			c, cancel := context.WithTimeout(ctx, 5*time.Second)
			t0 := time.Now()
			err := echo(c)
			d := time.Since(t0)
			cancel()
			if err != nil {
				t.Fatalf("urgent echo %d: %v", i, err)
			}
			lat = append(lat, d)
		}
		return lat
	}

	// Seek muxed onto the busy TCP bulk channel (head-of-line blocking).
	muxed := measure(func(c context.Context) error {
		_, err := bulk.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
		return err
	})
	// Seek on the QUIC channel: isolated from TCP bulk by transport (the fast lane).
	fast := measure(func(c context.Context) error {
		_, err := inter.Echo(c, &pb.EchoRequest{Payload: []byte("u")})
		return err
	})

	t.Logf("seek under saturating bulk (loopback):")
	t.Logf("  muxed on TCP bulk (HoL): p50=%v p99=%v max=%v", pctile(muxed, .5), pctile(muxed, .99), pctile(muxed, 1))
	t.Logf("  QUIC fast lane         : p50=%v p99=%v max=%v", pctile(fast, .5), pctile(fast, .99), pctile(fast, 1))
	if pctile(fast, .5) > pctile(muxed, .5) {
		t.Logf("  note: fast-lane p50 not lower on loopback (no bottleneck to congest; WAN is where it shows)")
	}
}

// TestManagerBulkReconstructOnMigrate: after the QUIC control plane migrates, the TCP
// bulk channel (whose 5-tuple died) is reconstructed, works again, and is a different
// connection than before.
func TestManagerBulkReconstructOnMigrate(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	quicAddr, tcpAddr, stop := twoChannelServers(t, serverID, clientID.Fingerprint())
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := DialControl(ctx, quicAddr, clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, tcpAddr); err != nil {
		t.Fatalf("DialBulk: %v", err)
	}

	before := m.BulkConn()
	if _, err := pb.NewBenchServiceClient(before).Echo(ctx, &pb.EchoRequest{Payload: []byte("pre")}); err != nil {
		t.Fatalf("bulk echo before migration: %v", err)
	}

	if err := m.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	after := m.BulkConn()
	if after == nil {
		t.Fatal("bulk channel was not reconstructed after migration")
	}
	if after == before {
		t.Fatal("bulk channel is the same conn; it was not reconstructed")
	}
	if _, err := pb.NewBenchServiceClient(after).Echo(ctx, &pb.EchoRequest{Payload: []byte("post")}); err != nil {
		t.Fatalf("bulk echo after reconstruction: %v", err)
	}
}

// TestBulkResumeAcrossReconstruct proves resume-by-offset: a bulk transfer interrupted
// mid-flight by a channel reconstruction resumes from the last received offset. Asserts
// completion, byte-correctness, contiguous offsets, and that the server saw StartOffset
// > 0 (a real resume, not a silent restart).
func TestBulkResumeAcrossReconstruct(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()

	qsrv, qaddr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		t.Fatalf("quic server: %v", err)
	}
	defer qsrv.Stop()
	var maxStart atomic.Uint64
	tsrv, taddr, err := ServeGRPCKDOver(TCP(), "127.0.0.1:0", benchService{maxStart: &maxStart}, serverID, clientID.Fingerprint())
	if err != nil {
		qsrv.Stop()
		t.Fatalf("tcp server: %v", err)
	}
	defer tsrv.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m, err := DialControl(ctx, qaddr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("DialControl: %v", err)
	}
	defer m.Close()
	if err := m.DialBulk(ctx, taddr.String()); err != nil {
		t.Fatalf("DialBulk: %v", err)
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
		t.Fatalf("BulkGet: %v", err)
	}
	if received != total {
		t.Fatalf("incomplete transfer: got %d want %d", received, total)
	}
	if maxStart.Load() == 0 {
		t.Fatal("no resume happened: server never saw StartOffset > 0 (restart, or never interrupted)")
	}
	t.Logf("interrupted mid-transfer, resumed from offset %d of %d, completed byte-correct", maxStart.Load(), uint64(total))
}
