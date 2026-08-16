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

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
	"github.com/stretchr/testify/require"
)

// BenchmarkEcho measures unary round-trip latency over each transport, reporting mean
// (ns/op) plus p50/p90/p99. On loopback this is the overhead tax of userspace QUIC vs
// kernel TCP, so QUIC is expected slower here; its advantage needs real latency/loss.
func BenchmarkEcho(b *testing.B) {
	for _, tr := range transports {
		b.Run(tr.name, func(b *testing.B) {
			client, cleanup := tr.newClient(b)
			defer cleanup()
			ctx := context.Background()
			req := &pb.EchoRequest{Payload: make([]byte, 64)}

			for i := 0; i < 50; i++ { // warm up: TLS/QUIC handshake, BDP ramp
				_, err := client.Echo(ctx, req)
				require.NoError(b, err, "warmup")
			}

			samples := make([]time.Duration, b.N)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				t0 := time.Now()
				if _, err := client.Echo(ctx, req); err != nil {
					b.Fatalf("echo: %v", err)
				}
				samples[i] = time.Since(t0)
			}
			b.StopTimer()
			reportLatency(b, samples)
		})
	}
}

// BenchmarkDownload measures streaming throughput (MB/s via b.SetBytes) over each
// transport. Same loopback caveat as BenchmarkEcho.
func BenchmarkDownload(b *testing.B) {
	const total = 64 << 20 // 64 MiB per op
	for _, tr := range transports {
		b.Run(tr.name, func(b *testing.B) {
			client, cleanup := tr.newClient(b)
			defer cleanup()
			ctx := context.Background()

			b.SetBytes(total)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: total, ChunkSize: 64 << 10})
				if err != nil {
					b.Fatalf("download: %v", err)
				}
				got, err := drainDownload(stream, nil)
				if err != nil {
					b.Fatalf("recv: %v", err)
				}
				if got != total {
					b.Fatalf("short read: %d/%d", got, total)
				}
			}
		})
	}
}

func reportLatency(b *testing.B, s []time.Duration) {
	if len(s) == 0 {
		return
	}
	pctUS := func(p float64) float64 { return float64(pctile(s, p).Microseconds()) }
	b.ReportMetric(pctUS(0.50), "p50-us")
	b.ReportMetric(pctUS(0.90), "p90-us")
	b.ReportMetric(pctUS(0.99), "p99-us")
}
