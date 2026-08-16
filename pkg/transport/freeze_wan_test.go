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
	"math/rand"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/benchmark/latency"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// motelWAN emulates a measured real link: ~60 Mbps bottleneck, ~80 ms RTT. Latency is
// one-way in this shaper, so 40 ms models the 80 ms round trip.
var motelWAN = latency.Network{Kbps: 60 * 1024, Latency: 40 * time.Millisecond, MTU: 1500}

// shapedTCP wraps each plain-TCP conn in a latency.Network so the handshake, gRPC, and
// record layer all run across the emulated WAN. Both ends must be wrapped, which dialing
// and listening through the same Network guarantees.
type shapedTCP struct{ net latency.Network }

func (s shapedTCP) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sc, err := s.net.Conn(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	return sc, nil
}

func (s shapedTCP) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return s.net.Listener(ln), nil
}

// TestWANFreezeUnderPrefetch reproduces a random-jump cache-miss read while a prefetch
// saturates the WAN link, contrasting two designs: muxed (read shares one gRPC channel
// with the bulk prefetch) vs fast lane (read on a separate channel). The shaper models
// latency and bandwidth but NOT loss, so it shows only the bandwidth-contention share;
// both channels are shaped TCP here, so the isolated variable is channel separation.
func TestWANFreezeUnderPrefetch(t *testing.T) {
	if testing.Short() {
		t.Skip("WAN freeze benchmark is slow by design (seconds-long muxed reads); run without -short")
	}

	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	tr := shapedTCP{net: motelWAN}

	// KeibiDrop's real gRPC tuning: a fixed 16 MiB window (autotuning off), so the
	// prefetch keeps ~16 MiB in flight (~2 s of buffered bulk a muxed read waits behind).
	srvOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(config.GRPCMaxMsgSize),
		grpc.MaxSendMsgSize(config.GRPCMaxMsgSize),
		grpc.InitialWindowSize(config.GRPCWindowSize),
		grpc.InitialConnWindowSize(config.GRPCWindowSize),
		grpc.WriteBufferSize(config.GRPCIOBufferSize),
		grpc.ReadBufferSize(config.GRPCIOBufferSize),
	}
	cliOpts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(config.GRPCMaxMsgSize), grpc.MaxCallSendMsgSize(config.GRPCMaxMsgSize)),
		grpc.WithInitialWindowSize(config.GRPCWindowSize),
		grpc.WithInitialConnWindowSize(config.GRPCWindowSize),
		grpc.WithWriteBufferSize(config.GRPCIOBufferSize),
		grpc.WithReadBufferSize(config.GRPCIOBufferSize),
	}

	testkit.Run(t, func() error {
		// One peer serving both the bulk prefetch and the on-demand reads.
		srv, addr := testkit.Must2(ServeGRPCKDOver(tr, "127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint(), srvOpts...))
		defer srv.Stop()

		ctx := context.Background()

		// Channel A carries the prefetch (and, in the muxed case, the reads too).
		bulkCh := testkit.Must(DialGRPCKDOver(tr, addr.String(), clientID, serverID.Fingerprint(), cliOpts...))
		defer bulkCh.Close()
		// Channel B is the separate fast lane.
		fastCh := testkit.Must(DialGRPCKDOver(tr, addr.String(), clientID, serverID.Fingerprint(), cliOpts...))
		defer fastCh.Close()

		const readLen = 512 << 10  // KeibiDrop's block: the first chunk fetched on a miss
		const fileBlocks = 1 << 14 // 8 GiB of virtual file to jump around in
		rng := rand.New(rand.NewSource(42))

		// The timed section below (t0 to time.Since(t0)) brackets only the RPC call.
		// Verification runs after d is captured, so it never perturbs the measurement.
		read := func(cc pb.BenchServiceClient, off uint64) (time.Duration, error) {
			c, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			t0 := time.Now()
			reply, err := cc.Read(c, &pb.ReadRequest{Offset: off, Length: readLen})
			d := time.Since(t0)
			if err != nil {
				return d, err
			}
			if len(reply.Data) != readLen || !verifyRange(reply.Data, off) {
				return d, fmt.Errorf("read at %d: wrong bytes (len %d)", off, len(reply.Data))
			}
			return d, nil
		}
		bulk := pb.NewBenchServiceClient(bulkCh)
		fast := pb.NewBenchServiceClient(fastCh)

		// Reference: a cache-miss read on an idle link, the floor (one round trip plus the transfer).
		idle, err := measureReads(t, 5, func(i int) (time.Duration, error) {
			return read(bulk, uint64(rng.Intn(fileBlocks))*readLen)
		})
		if err != nil {
			return err
		}

		// Start the prefetch saturating channel A, then let it fill the pipe.
		stop := saturateBulk(ctx, bulk, 256<<20)
		defer stop()
		time.Sleep(1 * time.Second) // let the prefetch saturate the link

		// The freeze: random-jump reads muxed onto the busy bulk channel.
		muxed, err := measureReads(t, 6, func(i int) (time.Duration, error) {
			return read(bulk, uint64(rng.Intn(fileBlocks))*readLen)
		})
		if err != nil {
			return err
		}
		// The fix: the same reads on the separate fast lane.
		lane, err := measureReads(t, 10, func(i int) (time.Duration, error) {
			return read(fast, uint64(rng.Intn(fileBlocks))*readLen)
		})
		if err != nil {
			return err
		}

		t.Logf("motel WAN (60 Mbps, ~80 ms RTT, NO loss), 512 KiB cache-miss read, random jumps:")
		t.Logf("  idle (no prefetch)           : p50=%v  max=%v", pctile(idle, .5), pctile(idle, 1))
		t.Logf("  muxed on prefetch (today)    : p50=%v  p99=%v  max=%v", pctile(muxed, .5), pctile(muxed, .99), pctile(muxed, 1))
		t.Logf("  separate fast lane (the fix) : p50=%v  p99=%v  max=%v", pctile(lane, .5), pctile(lane, .99), pctile(lane, 1))
		t.Logf("  contention factor (muxed/lane p50): %.1fx", float64(pctile(muxed, .5))/float64(pctile(lane, .5)))
		t.Logf("  NOTE: shaper models latency+bandwidth but NOT loss, so this is only the")
		t.Logf("  bandwidth-contention share. Verified empirically: even with KeibiDrop's real")
		t.Logf("  16 MiB fixed window the muxed read stays sub-second here, because gRPC's")
		t.Logf("  loopyWriter fair-schedules streams as the window drains. The seconds-long")
		t.Logf("  freeze is loss-driven TCP head-of-line blocking; real-WAN magnitude is 3066")
		t.Logf("  ms vs 109 ms (blog, 72 ms lossy link). Reproduce that on the lossy fleet.")

		// What this loss-free emulation can assert: the separate lane stays near the idle
		// floor (isolated from the prefetch), and muxing onto the busy channel is worse.
		return fp.All(
			fp.True("fast lane stays near the idle floor (isolated from bulk)", pctile(lane, .5) <= 2*pctile(idle, .5)),
			fp.True("muxing onto the prefetch channel is slower than the separate lane", pctile(muxed, .5) > pctile(lane, .5)),
		)
	})
}

func measureReads(t *testing.T, n int, do func(i int) (time.Duration, error)) ([]time.Duration, error) {
	t.Helper()
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		d, err := do(i)
		if err != nil {
			return nil, fmt.Errorf("read %d: %w", i, err)
		}
		out = append(out, d)
	}
	return out, nil
}
