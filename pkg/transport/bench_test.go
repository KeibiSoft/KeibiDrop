package transport

import (
	"context"
	"io"
	"sort"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// BenchmarkEcho measures unary round-trip latency over each transport, reporting
// the mean (ns/op) plus p50/p90/p99 as custom metrics.
//
// IMPORTANT: on loopback with zero loss this is the *overhead tax* of userspace
// QUIC + HTTP/2-over-a-QUIC-stream versus kernel TCP. QUIC is expected to be
// slower here. Its advantage appears under real latency/loss and on an address
// change — measured on the WAN (step 8), not on this benchmark.
func BenchmarkEcho(b *testing.B) {
	for _, tr := range transports {
		b.Run(tr.name, func(b *testing.B) {
			client, cleanup := tr.newClient(b)
			defer cleanup()
			ctx := context.Background()
			req := &pb.EchoRequest{Payload: make([]byte, 64)}

			for i := 0; i < 50; i++ { // warm up: TLS/QUIC handshake, BDP ramp
				if _, err := client.Echo(ctx, req); err != nil {
					b.Fatalf("warmup: %v", err)
				}
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
				var got uint64
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						b.Fatalf("recv: %v", err)
					}
					got += uint64(len(chunk.Data))
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
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	pctUS := func(p float64) float64 {
		idx := int(p * float64(len(s)-1))
		return float64(s[idx].Microseconds())
	}
	b.ReportMetric(pctUS(0.50), "p50-us")
	b.ReportMetric(pctUS(0.90), "p90-us")
	b.ReportMetric(pctUS(0.99), "p99-us")
}
