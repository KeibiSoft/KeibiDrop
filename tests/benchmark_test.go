// ABOUTME: End-to-end transfer benchmarks and baseline performance measurements.
// ABOUTME: Measures peer-to-peer throughput, per-chunk latency, and overhead ratios.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---------------------------------------------------------------------------
// Benchmark 1: End-to-End Transfer Throughput
// ---------------------------------------------------------------------------

// TestTransferThroughput measures the actual peer-to-peer transfer speed that
// users experience: Bob shares a file, Alice reads it through her FUSE mount.
func TestTransferThroughput(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping transfer throughput in short mode")
	}

	tp := SetupFUSEPeerPair(t, 300*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	t.Log("\n=== End-to-End Transfer Throughput ===")
	t.Logf("%-8s | %-12s | %-10s", "Size", "MB/s", "Duration")
	t.Logf("---------|--------------|----------")

	for _, s := range sizes {
		t.Run(s.name, func(t *testing.T) {
			require := require.New(t)

			// Generate random payload
			data := make([]byte, s.size)
			_, err := rand.Read(data)
			require.NoError(err)

			// Bob writes file to his save dir and shares it
			fileName := fmt.Sprintf("bench_e2e_%s.bin", s.name)
			bobPath := filepath.Join(tp.BobSaveDir, fileName)
			require.NoError(os.WriteFile(bobPath, data, 0644))
			require.NoError(tp.Bob.AddFile(bobPath))

			// Wait for Alice to see the file on her FUSE mount
			alicePath := filepath.Join(tp.AliceMountDir, fileName)
			WaitForFileOnMount(t, alicePath, 30*time.Second)

			// Timed read — this is the number users experience
			start := time.Now()
			readData, err := os.ReadFile(alicePath)
			elapsed := time.Since(start)
			require.NoError(err)
			require.Equal(len(data), len(readData), "size mismatch")

			// Verify content integrity for smallest size (comparing 100MB is slow).
			if s.size <= 1*1024*1024 {
				require.Equal(data, readData, "content mismatch")
			}

			mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("%-8s | %-12.2f | %s", s.name, mbps, elapsed.Round(time.Millisecond))
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmark 1b: Transfer Throughput With Simulated Network Latency
// ---------------------------------------------------------------------------

// netemProfile defines a simulated network condition applied via tc netem.
type netemProfile struct {
	name    string
	delay   string // one-way delay (RTT = 2x)
	jitter  string // optional jitter
	rate    string // optional bandwidth limit
}

var netemProfiles = []netemProfile{
	{"LAN_1ms", "500us", "100us", ""},             // ~1ms RTT, gigabit LAN
	{"WiFi_5ms", "2500us", "500us", ""},            // ~5ms RTT, WiFi on same network
	{"LAN_1ms_100Mbps", "500us", "100us", "100mbit"}, // ~1ms RTT, 100 Mbps link
}

// applyNetem adds a tc netem qdisc to the loopback interface.
// Returns a cleanup function that removes it.
func applyNetem(t *testing.T, p netemProfile) func() {
	t.Helper()

	// Remove any stale qdisc from a previous run before adding ours.
	exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()

	args := []string{"qdisc", "add", "dev", "lo", "root", "handle", "1:", "netem",
		"delay", p.delay}
	if p.jitter != "" {
		args = append(args, p.jitter)
	}

	out, err := exec.Command("tc", args...).CombinedOutput()
	if err != nil {
		t.Skipf("tc netem failed (need sudo): %s: %v", string(out), err)
	}

	// If bandwidth limit requested, add a child tbf qdisc
	if p.rate != "" {
		tbfArgs := []string{"qdisc", "add", "dev", "lo", "parent", "1:", "handle", "2:",
			"tbf", "rate", p.rate, "burst", "256kb", "latency", "50ms"}
		out, err := exec.Command("tc", tbfArgs...).CombinedOutput()
		if err != nil {
			// Clean up the netem qdisc before skipping
			exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()
			t.Skipf("tc tbf failed: %s: %v", string(out), err)
		}
	}

	return func() {
		exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()
	}
}

// TestTransferThroughputNetem measures E2E transfer with simulated network
// conditions. Requires Linux with tc netem.
// Requires KEIBIDROP_BENCH_NETEM=1 and sudo/CAP_NET_ADMIN.
//
// Run with: sudo -E KEIBIDROP_BENCH_NETEM=1 go test -run=TestTransferThroughputNetem -v -timeout=600s ./tests/...
func TestTransferThroughputNetem(t *testing.T) {
	if os.Getenv("KEIBIDROP_BENCH_NETEM") != "1" {
		t.Skip("set KEIBIDROP_BENCH_NETEM=1 to run (requires sudo or CAP_NET_ADMIN)")
	}
	skipIfNoFUSE(t)

	sizes := []struct {
		name string
		size int
	}{
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"600MB", 600 * 1024 * 1024},
	}

	for _, profile := range netemProfiles {
		t.Run(profile.name, func(t *testing.T) {
			tp := SetupFUSEPeerPair(t, 600*time.Second)
			waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

			cleanup := applyNetem(t, profile)
			t.Cleanup(cleanup)

			desc := fmt.Sprintf("delay=%s jitter=%s", profile.delay, profile.jitter)
			if profile.rate != "" {
				desc += fmt.Sprintf(" rate=%s", profile.rate)
			}
			t.Logf("\n=== Transfer with netem: %s (%s) ===", profile.name, desc)
			t.Logf("%-8s | %-12s | %-10s", "Size", "MB/s", "Duration")
			t.Logf("---------|--------------|----------")

			for _, s := range sizes {
				t.Run(s.name, func(t *testing.T) {
					require := require.New(t)

					data := make([]byte, s.size)
					_, err := rand.Read(data)
					require.NoError(err)

					fileName := fmt.Sprintf("bench_netem_%s_%s.bin", profile.name, s.name)
					bobPath := filepath.Join(tp.BobSaveDir, fileName)
					require.NoError(os.WriteFile(bobPath, data, 0644))
					require.NoError(tp.Bob.AddFile(bobPath))

					alicePath := filepath.Join(tp.AliceMountDir, fileName)
					WaitForFileOnMount(t, alicePath, 60*time.Second)

					start := time.Now()
					readData, err := os.ReadFile(alicePath)
					elapsed := time.Since(start)
					require.NoError(err)
					require.Equal(len(data), len(readData), "size mismatch")

					mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
					t.Logf("%-8s | %-12.2f | %s", s.name, mbps, elapsed.Round(time.Millisecond))
				})
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmark 2: Per-Component Latency Breakdown
// ---------------------------------------------------------------------------

// chunkTiming records the duration of a single ReadAt call.
type chunkTiming struct {
	Offset   int64
	Size     int64
	Duration time.Duration
}

// timedStream wraps a RemoteFileStream and records per-chunk timing.
type timedStream struct {
	inner    types.RemoteFileStream
	recorder *chunkRecorder
}

func (ts *timedStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	start := time.Now()
	data, err := ts.inner.ReadAt(ctx, offset, size)
	elapsed := time.Since(start)
	if err == nil {
		ts.recorder.record(chunkTiming{Offset: offset, Size: size, Duration: elapsed})
	}
	return data, err
}

func (ts *timedStream) Close() error {
	return ts.inner.Close()
}

// chunkRecorder collects chunk timings from concurrent streams.
type chunkRecorder struct {
	mu     sync.Mutex
	chunks []chunkTiming
}

func (cr *chunkRecorder) record(ct chunkTiming) {
	cr.mu.Lock()
	cr.chunks = append(cr.chunks, ct)
	cr.mu.Unlock()
}

func (cr *chunkRecorder) results() []chunkTiming {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	out := make([]chunkTiming, len(cr.chunks))
	copy(out, cr.chunks)
	return out
}

// timedStreamProvider wraps a FileStreamProvider factory and injects timing.
type timedStreamProvider struct {
	inner    types.FileStreamProvider
	recorder *chunkRecorder
}

func (tsp *timedStreamProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	stream, err := tsp.inner.OpenRemoteFile(ctx, inode, path)
	if err != nil {
		return nil, err
	}
	return &timedStream{inner: stream, recorder: tsp.recorder}, nil
}

func (tsp *timedStreamProvider) StreamFile(ctx context.Context, path string, startOffset uint64) (types.StreamFileReceiver, error) {
	return tsp.inner.StreamFile(ctx, path, startOffset)
}

// TestChunkLatency measures per-chunk ReadAt latency during a transfer,
// reporting min/median/p95/max statistics.
func TestChunkLatency(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping chunk latency in short mode")
	}

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// Inject timed stream provider into Alice's FUSE root.
	// Safe to mutate after mount: no remote files exist yet, so no streams
	// are open. The factory is called lazily when a remote file is opened.
	recorder := &chunkRecorder{}
	originalFactory := tp.Alice.FS.Root.OpenStreamProvider
	tp.Alice.FS.Root.OpenStreamProvider = func() types.FileStreamProvider {
		return &timedStreamProvider{inner: originalFactory(), recorder: recorder}
	}

	// Bob shares a 10 MB file
	require := require.New(t)
	fileSize := 10 * 1024 * 1024
	data := make([]byte, fileSize)
	_, err := rand.Read(data)
	require.NoError(err)

	bobPath := filepath.Join(tp.BobSaveDir, "bench_latency.bin")
	require.NoError(os.WriteFile(bobPath, data, 0644))
	require.NoError(tp.Bob.AddFile(bobPath))

	// Alice reads via FUSE — this triggers the timed ReadAt calls
	alicePath := filepath.Join(tp.AliceMountDir, "bench_latency.bin")
	WaitForFileOnMount(t, alicePath, 30*time.Second)

	start := time.Now()
	readData, err := os.ReadFile(alicePath)
	totalElapsed := time.Since(start)
	require.NoError(err)
	require.Equal(fileSize, len(readData))

	// Analyze chunk timings
	chunks := recorder.results()
	if len(chunks) == 0 {
		t.Fatal("no chunk timings recorded — stream provider was not used")
	}

	durations := make([]time.Duration, len(chunks))
	var totalChunkTime time.Duration
	for i, c := range chunks {
		durations[i] = c.Duration
		totalChunkTime += c.Duration
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })

	n := len(durations)
	pct := func(p float64) time.Duration {
		idx := int(float64(n-1) * p)
		if idx >= n {
			idx = n - 1
		}
		return durations[idx]
	}

	mbps := float64(fileSize) / totalElapsed.Seconds() / (1024 * 1024)

	t.Log("\n=== Per-Chunk Latency Breakdown (10 MB transfer) ===")
	t.Logf("Chunks recorded:    %d", n)
	t.Logf("Total wall-clock:   %s", totalElapsed.Round(time.Millisecond))
	t.Logf("Sum of chunk times: %s", totalChunkTime.Round(time.Millisecond))
	t.Logf("Effective MB/s:     %.2f", mbps)
	t.Log("")
	t.Logf("%-10s | %-15s", "Percentile", "Latency")
	t.Logf("-----------|----------------")
	t.Logf("%-10s | %s", "Min", durations[0].Round(time.Microsecond))
	t.Logf("%-10s | %s", "Median", pct(0.50).Round(time.Microsecond))
	t.Logf("%-10s | %s", "P95", pct(0.95).Round(time.Microsecond))
	t.Logf("%-10s | %s", "Max", durations[n-1].Round(time.Microsecond))
}

// ---------------------------------------------------------------------------
// Benchmark 3a: Raw Local Disk I/O Baseline
// ---------------------------------------------------------------------------

// BenchmarkLocalDisk measures raw local disk throughput for comparison.
func BenchmarkLocalDisk(b *testing.B) {
	tmpDir, err := os.MkdirTemp("", "keibidrop-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"10KB", 10 * 1024},
		{"100KB", 100 * 1024},
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)

		b.Run(fmt.Sprintf("Write_%s", s.name), func(b *testing.B) {
			b.SetBytes(int64(s.size))
			// Reuse same path to avoid filling disk on large sizes.
			writePath := filepath.Join(tmpDir, fmt.Sprintf("bench_w_%s.bin", s.name))
			for i := 0; i < b.N; i++ {
				if err := os.WriteFile(writePath, data, 0644); err != nil {
					b.Fatal(err)
				}
			}
		})

		readPath := filepath.Join(tmpDir, fmt.Sprintf("bench_r_%s.bin", s.name))
		os.WriteFile(readPath, data, 0644)

		b.Run(fmt.Sprintf("Read_%s", s.name), func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for i := 0; i < b.N; i++ {
				if _, err := os.ReadFile(readPath); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmark 3b: gRPC Roundtrip Without Encryption
// ---------------------------------------------------------------------------

// bareReadServer is a minimal KeibiService that serves Read requests from a
// directory on disk, with no encryption or session management.
type bareReadServer struct {
	bindings.UnimplementedKeibiServiceServer
	dir string
}

func (s *bareReadServer) Read(stream bindings.KeibiService_ReadServer) error {
	var fh *os.File
	defer func() {
		if fh != nil {
			fh.Close()
		}
	}()

	buf := make([]byte, filesystem.ChunkSize)

	for {
		req, err := stream.Recv()
		if err != nil {
			return nil // client closed
		}

		if fh == nil {
			fh, err = os.Open(filepath.Join(s.dir, req.Path))
			if err != nil {
				return err
			}
		}

		readSize := int(req.Size)
		if readSize > len(buf) {
			readSize = len(buf)
		}

		n, err := fh.ReadAt(buf[:readSize], int64(req.Offset))
		if err != nil && n == 0 {
			return err
		}

		if err := stream.Send(&bindings.ReadResponse{Data: buf[:n]}); err != nil {
			return err
		}
	}
}

// BenchmarkGRPCBaseline measures raw gRPC bidi-stream throughput over
// localhost without encryption, using the KeibiService Read RPC.
func BenchmarkGRPCBaseline(b *testing.B) {
	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	// Create temp dir with test files
	tmpDir, err := os.MkdirTemp("", "keibidrop-grpc-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)
		os.WriteFile(filepath.Join(tmpDir, s.name+".bin"), data, 0644)
	}

	// Start bare gRPC server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	srv := grpc.NewServer()
	bindings.RegisterKeibiServiceServer(srv, &bareReadServer{dir: tmpDir})
	go srv.Serve(lis)
	defer srv.Stop()

	// Connect client
	conn, err := grpc.NewClient(
		lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close()
	cli := bindings.NewKeibiServiceClient(conn)

	chunkSize := filesystem.ChunkSize

	for _, s := range sizes {
		b.Run(fmt.Sprintf("Read_%s", s.name), func(b *testing.B) {
			b.SetBytes(int64(s.size))
			for i := 0; i < b.N; i++ {
				stream, err := cli.Read(context.Background())
				if err != nil {
					b.Fatal(err)
				}

				offset := 0
				for offset < s.size {
					reqSize := chunkSize
					if offset+reqSize > s.size {
						reqSize = s.size - offset
					}
					if err := stream.Send(&bindings.ReadRequest{
						Path:   s.name + ".bin",
						Offset: uint64(offset),
						Size:   uint32(reqSize),
					}); err != nil {
						b.Fatal(err)
					}
					resp, err := stream.Recv()
					if err != nil {
						b.Fatal(err)
					}
					offset += len(resp.Data)
				}
				stream.CloseSend()
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmark 3c: gRPC Over Encrypted SecureConn
// ---------------------------------------------------------------------------

// TestEncryptedGRPC measures gRPC throughput through the full encrypted
// peer connection (no FUSE). Bob has the file, Alice reads via KDClient.
func TestEncryptedGRPC(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping encrypted gRPC throughput in short mode")
	}

	// No FUSE needed — just the encrypted gRPC channel between peers.
	tp := SetupPeerPairWithTimeout(t, false, 300*time.Second)

	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	chunkSize := filesystem.ChunkSize

	t.Log("\n=== Encrypted gRPC Throughput (no FUSE) ===")
	t.Logf("%-8s | %-12s | %-10s", "Size", "MB/s", "Duration")
	t.Logf("---------|--------------|----------")

	for _, s := range sizes {
		t.Run(s.name, func(t *testing.T) {
			require := require.New(t)

			data := make([]byte, s.size)
			_, err := rand.Read(data)
			require.NoError(err)

			fileName := fmt.Sprintf("bench_enc_%s.bin", s.name)
			bobPath := filepath.Join(tp.BobSaveDir, fileName)
			require.NoError(os.WriteFile(bobPath, data, 0644))
			require.NoError(tp.Bob.AddFile(bobPath))

			// Read directly via encrypted gRPC (no FUSE)
			start := time.Now()
			stream, err := tp.Alice.KDClient.Read(context.Background())
			require.NoError(err)

			totalRead := 0
			for totalRead < s.size {
				reqSize := chunkSize
				if totalRead+reqSize > s.size {
					reqSize = s.size - totalRead
				}
				require.NoError(stream.Send(&bindings.ReadRequest{
					Path:   fileName,
					Offset: uint64(totalRead),
					Size:   uint32(reqSize),
				}))
				resp, err := stream.Recv()
				require.NoError(err)
				totalRead += len(resp.Data)
			}
			stream.CloseSend()
			elapsed := time.Since(start)

			mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("%-8s | %-12.2f | %s", s.name, mbps, elapsed.Round(time.Millisecond))
		})
	}
}

// ---------------------------------------------------------------------------
// Overhead Ratio Comparison
// ---------------------------------------------------------------------------

// TestBaselineComparison runs a single transfer at each size and prints
// a ratio table showing how much overhead the full pipeline adds compared
// to raw disk and raw gRPC.
func TestBaselineComparison(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping baseline comparison in short mode")
	}

	tp := SetupFUSEPeerPair(t, 300*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	type result struct {
		disk    time.Duration
		e2e     time.Duration
	}

	t.Log("\n=== Baseline Comparison: E2E vs Raw Disk ===")
	t.Logf("%-8s | %-12s | %-12s | %-10s | %-8s",
		"Size", "Raw Disk", "E2E Transfer", "Overhead", "Ratio")
	t.Logf("---------|--------------|--------------|------------|--------")

	tmpDir := t.TempDir()

	for _, s := range sizes {
		require := require.New(t)
		data := make([]byte, s.size)
		_, err := rand.Read(data)
		require.NoError(err)

		// Measure raw disk read
		diskPath := filepath.Join(tmpDir, fmt.Sprintf("baseline_%s.bin", s.name))
		require.NoError(os.WriteFile(diskPath, data, 0644))

		diskStart := time.Now()
		_, err = os.ReadFile(diskPath)
		diskDur := time.Since(diskStart)
		require.NoError(err)

		// Measure E2E transfer
		fileName := fmt.Sprintf("bench_cmp_%s.bin", s.name)
		bobPath := filepath.Join(tp.BobSaveDir, fileName)
		require.NoError(os.WriteFile(bobPath, data, 0644))
		require.NoError(tp.Bob.AddFile(bobPath))

		alicePath := filepath.Join(tp.AliceMountDir, fileName)
		WaitForFileOnMount(t, alicePath, 30*time.Second)

		e2eStart := time.Now()
		readData, err := os.ReadFile(alicePath)
		e2eDur := time.Since(e2eStart)
		require.NoError(err)
		require.Equal(s.size, len(readData))

		r := result{disk: diskDur, e2e: e2eDur}
		ratio := float64(r.e2e) / float64(r.disk)
		overhead := r.e2e - r.disk

		t.Logf("%-8s | %-12s | %-12s | %-10s | %.1fx",
			s.name,
			r.disk.Round(time.Millisecond),
			r.e2e.Round(time.Millisecond),
			overhead.Round(time.Millisecond),
			ratio)
	}
}

// ---------------------------------------------------------------------------
// Benchmark 5: FUSE Read Overhead Breakdown
// ---------------------------------------------------------------------------

// TestFUSEReadOverhead measures where time is spent in the E2E FUSE read path
// by comparing layers: raw gRPC, gRPC+copy, gRPC+copy+cachewrite, full FUSE.
func TestFUSEReadOverhead(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping FUSE read overhead in short mode")
	}

	fileSize := 100 * 1024 * 1024 // 100 MB
	require := require.New(t)

	data := make([]byte, fileSize)
	_, err := rand.Read(data)
	require.NoError(err)

	// --- Layer 1: Raw encrypted gRPC (no FUSE, no cache writes) ---
	tp1 := SetupPeerPairWithTimeout(t, false, 120*time.Second)
	bobPath1 := filepath.Join(tp1.BobSaveDir, "overhead_enc.bin")
	require.NoError(os.WriteFile(bobPath1, data, 0644))
	require.NoError(tp1.Bob.AddFile(bobPath1))

	chunkSize := filesystem.ChunkSize
	start := time.Now()
	stream, err := tp1.Alice.KDClient.Read(context.Background())
	require.NoError(err)
	totalRead := 0
	for totalRead < fileSize {
		reqSize := chunkSize
		if totalRead+reqSize > fileSize {
			reqSize = fileSize - totalRead
		}
		require.NoError(stream.Send(&bindings.ReadRequest{
			Path: "overhead_enc.bin", Offset: uint64(totalRead), Size: uint32(reqSize),
		}))
		resp, err := stream.Recv()
		require.NoError(err)
		totalRead += len(resp.Data)
	}
	stream.CloseSend()
	encGRPC := time.Since(start)

	// --- Layer 2: Encrypted gRPC + copy into user buffer (simulates FUSE copy) ---
	tp2 := SetupPeerPairWithTimeout(t, false, 120*time.Second)
	bobPath2 := filepath.Join(tp2.BobSaveDir, "overhead_copy.bin")
	require.NoError(os.WriteFile(bobPath2, data, 0644))
	require.NoError(tp2.Bob.AddFile(bobPath2))

	userBuf := make([]byte, chunkSize)
	start = time.Now()
	stream2, err := tp2.Alice.KDClient.Read(context.Background())
	require.NoError(err)
	totalRead = 0
	for totalRead < fileSize {
		reqSize := chunkSize
		if totalRead+reqSize > fileSize {
			reqSize = fileSize - totalRead
		}
		require.NoError(stream2.Send(&bindings.ReadRequest{
			Path: "overhead_copy.bin", Offset: uint64(totalRead), Size: uint32(reqSize),
		}))
		resp, err := stream2.Recv()
		require.NoError(err)
		copy(userBuf, resp.Data)
		totalRead += len(resp.Data)
	}
	stream2.CloseSend()
	encGRPCCopy := time.Since(start)

	// --- Layer 3: Encrypted gRPC + copy + cache write (pwrite to temp file) ---
	tp3 := SetupPeerPairWithTimeout(t, false, 120*time.Second)
	bobPath3 := filepath.Join(tp3.BobSaveDir, "overhead_cache.bin")
	require.NoError(os.WriteFile(bobPath3, data, 0644))
	require.NoError(tp3.Bob.AddFile(bobPath3))

	cacheFile, err := os.CreateTemp("", "overhead-cache-*.bin")
	require.NoError(err)
	require.NoError(cacheFile.Truncate(int64(fileSize)))
	defer os.Remove(cacheFile.Name())
	defer cacheFile.Close()

	start = time.Now()
	stream3, err := tp3.Alice.KDClient.Read(context.Background())
	require.NoError(err)
	totalRead = 0
	for totalRead < fileSize {
		reqSize := chunkSize
		if totalRead+reqSize > fileSize {
			reqSize = fileSize - totalRead
		}
		require.NoError(stream3.Send(&bindings.ReadRequest{
			Path: "overhead_cache.bin", Offset: uint64(totalRead), Size: uint32(reqSize),
		}))
		resp, err := stream3.Recv()
		require.NoError(err)
		copy(userBuf, resp.Data)
		_, err = cacheFile.WriteAt(resp.Data, int64(totalRead))
		require.NoError(err)
		totalRead += len(resp.Data)
	}
	stream3.CloseSend()
	encGRPCCopyCache := time.Since(start)

	// --- Layer 4: Full E2E FUSE (includes kernel transitions, bitmap, etc.) ---
	tp4 := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp4.AliceMountDir, 15*time.Second)

	bobPath4 := filepath.Join(tp4.BobSaveDir, "overhead_fuse.bin")
	require.NoError(os.WriteFile(bobPath4, data, 0644))
	require.NoError(tp4.Bob.AddFile(bobPath4))

	alicePath := filepath.Join(tp4.AliceMountDir, "overhead_fuse.bin")
	WaitForFileOnMount(t, alicePath, 30*time.Second)

	start = time.Now()
	readData, err := os.ReadFile(alicePath)
	fuseE2E := time.Since(start)
	require.NoError(err)
	require.Equal(fileSize, len(readData))

	// --- Results ---
	mb := float64(fileSize) / (1024 * 1024)
	t.Log("\n=== FUSE Read Overhead Breakdown (100 MB) ===")
	t.Logf("%-35s | %-10s | %-10s | %-10s", "Layer", "Duration", "MB/s", "Delta")
	t.Logf("------------------------------------|------------|------------|----------")
	t.Logf("%-35s | %-10s | %-10.1f | -",
		"1. Encrypted gRPC (baseline)", encGRPC.Round(time.Millisecond), mb/encGRPC.Seconds())
	t.Logf("%-35s | %-10s | %-10.1f | +%s",
		"2. + copy into user buffer", encGRPCCopy.Round(time.Millisecond), mb/encGRPCCopy.Seconds(),
		(encGRPCCopy - encGRPC).Round(time.Millisecond))
	t.Logf("%-35s | %-10s | %-10.1f | +%s",
		"3. + pwrite to cache file", encGRPCCopyCache.Round(time.Millisecond), mb/encGRPCCopyCache.Seconds(),
		(encGRPCCopyCache - encGRPCCopy).Round(time.Millisecond))
	t.Logf("%-35s | %-10s | %-10.1f | +%s",
		"4. Full FUSE E2E", fuseE2E.Round(time.Millisecond), mb/fuseE2E.Seconds(),
		(fuseE2E - encGRPCCopyCache).Round(time.Millisecond))
	t.Log("")
	t.Log("Delta breakdown:")
	t.Logf("  Copy overhead:        %s (%.1f%%)", (encGRPCCopy - encGRPC).Round(time.Millisecond),
		float64(encGRPCCopy-encGRPC)/float64(fuseE2E)*100)
	t.Logf("  Cache write overhead: %s (%.1f%%)", (encGRPCCopyCache - encGRPCCopy).Round(time.Millisecond),
		float64(encGRPCCopyCache-encGRPCCopy)/float64(fuseE2E)*100)
	t.Logf("  FUSE/kernel overhead: %s (%.1f%%)", (fuseE2E - encGRPCCopyCache).Round(time.Millisecond),
		float64(fuseE2E-encGRPCCopyCache)/float64(fuseE2E)*100)
}

// ---------------------------------------------------------------------------
// Benchmark 6: FUSE Write Throughput (copy files INTO the mounted filesystem)
// ---------------------------------------------------------------------------

// TestFUSEWriteThroughput measures how fast files can be written into the FUSE
// mount from outside (simulating drag-and-drop or cp). This exercises Create,
// Write, Flush, Release, and the peer notification path.
func TestFUSEWriteThroughput(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping FUSE write throughput in short mode")
	}

	tp := SetupFUSEPeerPair(t, 300*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// Single file write throughput at various sizes.
	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	t.Log("\n=== FUSE Write Throughput (single file) ===")
	t.Logf("%-8s | %-12s | %-10s", "Size", "MB/s", "Duration")
	t.Logf("---------|--------------|----------")

	for _, s := range sizes {
		t.Run("Single_"+s.name, func(t *testing.T) {
			require := require.New(t)

			data := make([]byte, s.size)
			_, err := rand.Read(data)
			require.NoError(err)

			destPath := filepath.Join(tp.AliceMountDir, fmt.Sprintf("write_bench_%s.bin", s.name))

			start := time.Now()
			err = os.WriteFile(destPath, data, 0644)
			elapsed := time.Since(start)
			require.NoError(err)

			mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("%-8s | %-12.2f | %s", s.name, mbps, elapsed.Round(time.Millisecond))

			// Verify
			readBack, err := os.ReadFile(destPath)
			require.NoError(err)
			require.Equal(len(data), len(readBack), "size mismatch")
		})
	}

	// Multi-file write: copy N files of a given size into the mount.
	multiTests := []struct {
		name      string
		fileCount int
		fileSize  int
	}{
		{"10x1MB", 10, 1 * 1024 * 1024},
		{"10x10MB", 10, 10 * 1024 * 1024},
		{"100x1MB", 100, 1 * 1024 * 1024},
		{"100x10MB", 100, 10 * 1024 * 1024},
	}

	t.Log("\n=== FUSE Write Throughput (multi-file) ===")
	t.Logf("%-12s | %-8s | %-12s | %-10s | %-12s", "Test", "Total", "MB/s", "Duration", "Per-file avg")
	t.Logf("-------------|----------|--------------|------------|------------")

	for _, mt := range multiTests {
		t.Run("Multi_"+mt.name, func(t *testing.T) {
			require := require.New(t)

			totalBytes := mt.fileCount * mt.fileSize
			data := make([]byte, mt.fileSize)
			_, err := rand.Read(data)
			require.NoError(err)

			// Create a subdirectory for this batch.
			batchDir := filepath.Join(tp.AliceMountDir, "batch_"+mt.name)
			require.NoError(os.MkdirAll(batchDir, 0755))

			start := time.Now()
			for i := 0; i < mt.fileCount; i++ {
				destPath := filepath.Join(batchDir, fmt.Sprintf("file_%04d.bin", i))
				err := os.WriteFile(destPath, data, 0644)
				require.NoError(err)
			}
			elapsed := time.Since(start)

			totalMB := float64(totalBytes) / (1024 * 1024)
			mbps := totalMB / elapsed.Seconds()
			perFile := elapsed / time.Duration(mt.fileCount)
			t.Logf("%-12s | %-8s | %-12.2f | %-10s | %s",
				mt.name,
				fmt.Sprintf("%.0fMB", totalMB),
				mbps,
				elapsed.Round(time.Millisecond),
				perFile.Round(time.Millisecond))
		})
	}
}

// ---------------------------------------------------------------------------
// Benchmark: Full Round-Trip (write on Alice FUSE -> sync to Bob -> verify)
// ---------------------------------------------------------------------------

func TestRoundTripFUSETransfer(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping round-trip FUSE benchmark in short mode")
	}

	tp := SetupFUSEPeerPair(t, 300*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	sizes := []struct {
		name string
		size int
	}{
		{"1MB", 1 * 1024 * 1024},
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	t.Log("\n=== Round-Trip FUSE Transfer ===")
	t.Log("Alice writes to her FUSE mount, file syncs to Bob, Bob reads full file from save dir.")
	t.Logf("%-8s | %-12s | %-12s | %-12s | %-10s", "Size", "Write MB/s", "Sync MB/s", "Total MB/s", "Duration")
	t.Logf("---------|--------------|--------------|--------------|----------")

	for _, s := range sizes {
		t.Run("RoundTrip_"+s.name, func(t *testing.T) {
			require := require.New(t)

			data := make([]byte, s.size)
			_, err := rand.Read(data)
			require.NoError(err)

			fileName := fmt.Sprintf("roundtrip_%s.bin", s.name)
			alicePath := filepath.Join(tp.AliceMountDir, fileName)

			totalStart := time.Now()

			// Phase 1: Alice writes to her FUSE mount
			writeStart := time.Now()
			err = os.WriteFile(alicePath, data, 0644)
			writeElapsed := time.Since(writeStart)
			require.NoError(err)

			// Phase 2: Wait for Bob to see the file notification
			WaitForRemoteFile(t, tp.Bob.SyncTracker, "/"+fileName, 30*time.Second)

			// Phase 3: Bob pulls the file (timed)
			pullStart := time.Now()
			bobPath := filepath.Join(tp.BobSaveDir, fileName)
			require.NoError(tp.Bob.PullFile("/"+fileName, bobPath))
			pullElapsed := time.Since(pullStart)

			totalElapsed := time.Since(totalStart)

			// Phase 4: Verify content
			bobData, err := os.ReadFile(bobPath)
			require.NoError(err)
			require.Equal(len(data), len(bobData), "size mismatch")
			if s.size <= 10*1024*1024 {
				require.Equal(data, bobData, "content mismatch")
			}

			writeMBps := float64(s.size) / writeElapsed.Seconds() / (1024 * 1024)
			pullMBps := float64(s.size) / pullElapsed.Seconds() / (1024 * 1024)
			totalMBps := float64(s.size) / totalElapsed.Seconds() / (1024 * 1024)
			t.Logf("%-8s | %-12.2f | %-12.2f | %-12.2f | %s",
				s.name, writeMBps, pullMBps, totalMBps, totalElapsed.Round(time.Millisecond))
		})
	}

	// Reverse direction: Bob adds file, Alice reads from FUSE mount
	t.Log("\n=== Reverse: Bob AddFile -> Alice reads from FUSE ===")
	t.Logf("%-8s | %-12s | %-10s", "Size", "Read MB/s", "Duration")
	t.Logf("---------|--------------|----------")

	for _, s := range sizes {
		t.Run("Reverse_"+s.name, func(t *testing.T) {
			require := require.New(t)

			data := make([]byte, s.size)
			_, err := rand.Read(data)
			require.NoError(err)

			fileName := fmt.Sprintf("reverse_%s.bin", s.name)
			bobPath := filepath.Join(tp.BobSaveDir, fileName)
			require.NoError(os.WriteFile(bobPath, data, 0644))
			require.NoError(tp.Bob.AddFile(bobPath))

			alicePath := filepath.Join(tp.AliceMountDir, fileName)
			WaitForFileOnMount(t, alicePath, 30*time.Second)

			start := time.Now()
			readData, err := os.ReadFile(alicePath)
			elapsed := time.Since(start)
			require.NoError(err)
			require.Equal(len(data), len(readData), "size mismatch")

			mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("%-8s | %-12.2f | %s", s.name, mbps, elapsed.Round(time.Millisecond))
		})
	}
}

// ---------------------------------------------------------------------------
// Legacy portable benchmarks (fixed from hardcoded Mac paths)
// ---------------------------------------------------------------------------

// TestMeasureLatency provides human-readable latency measurements for
// local FUSE operations (write + read at various sizes).
func TestMeasureLatency(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping latency test in short mode")
	}

	tp := SetupFUSEPeerPair(t, 60*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	mountPath := tp.AliceMountDir

	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"10KB", 10 * 1024},
		{"100KB", 100 * 1024},
		{"1MB", 1024 * 1024},
	}

	t.Log("\n=== FUSE Mount Latency Measurements ===")
	t.Logf("%-10s | %-15s | %-15s | %-15s", "Size", "Create+Write", "Read", "Total")
	t.Logf("%s", "----------|-----------------|-----------------|----------------")

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)
		path := filepath.Join(mountPath, fmt.Sprintf("latency_%s.bin", s.name))

		startWrite := time.Now()
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Logf("%-10s | WRITE ERROR: %v", s.name, err)
			continue
		}
		writeLatency := time.Since(startWrite)

		time.Sleep(100 * time.Millisecond)

		startRead := time.Now()
		readData, err := os.ReadFile(path)
		if err != nil {
			t.Logf("%-10s | OK | READ ERROR: %v", s.name, err)
			os.Remove(path)
			continue
		}
		readLatency := time.Since(startRead)

		if len(readData) != len(data) {
			t.Logf("%-10s | Size mismatch: wrote %d, read %d", s.name, len(data), len(readData))
		}

		t.Logf("%-10s | %-15s | %-15s | %-15s",
			s.name,
			writeLatency.Round(time.Microsecond),
			readLatency.Round(time.Microsecond),
			(writeLatency + readLatency).Round(time.Microsecond))

		os.Remove(path)
	}

	t.Log("\n=== Local Disk Baseline ===")
	tmpDir := t.TempDir()

	t.Logf("%-10s | %-15s | %-15s | %-15s", "Size", "Create+Write", "Read", "Total")
	t.Logf("%s", "----------|-----------------|-----------------|----------------")

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)
		path := filepath.Join(tmpDir, fmt.Sprintf("baseline_%s.bin", s.name))

		startWrite := time.Now()
		os.WriteFile(path, data, 0644)
		writeLatency := time.Since(startWrite)

		startRead := time.Now()
		os.ReadFile(path)
		readLatency := time.Since(startRead)

		t.Logf("%-10s | %-15s | %-15s | %-15s",
			s.name,
			writeLatency.Round(time.Microsecond),
			readLatency.Round(time.Microsecond),
			(writeLatency + readLatency).Round(time.Microsecond))
	}
}

// TestOpenCloseLatency measures file handle open/close operations on
// the FUSE mount.
func TestOpenCloseLatency(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping open/close latency test in short mode")
	}

	tp := SetupFUSEPeerPair(t, 60*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	path := filepath.Join(tp.AliceMountDir, "openclose_test.txt")
	os.WriteFile(path, []byte("test content"), 0644)
	defer os.Remove(path)

	time.Sleep(500 * time.Millisecond)

	t.Log("\n=== Open/Close Latency (100 iterations) ===")

	iterations := 100
	var totalOpen, totalClose time.Duration

	for i := 0; i < iterations; i++ {
		startOpen := time.Now()
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		totalOpen += time.Since(startOpen)

		startClose := time.Now()
		f.Close()
		totalClose += time.Since(startClose)
	}

	t.Logf("Average Open:  %v", totalOpen/time.Duration(iterations))
	t.Logf("Average Close: %v", totalClose/time.Duration(iterations))
	t.Logf("Total for %d iterations: Open=%v, Close=%v", iterations, totalOpen, totalClose)
}
