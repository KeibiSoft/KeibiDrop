// ABOUTME: pprof-instrumented PullFile profile and SecureConn throughput tests.
// ABOUTME: Provides CPU/heap profiles, cipher throughput, and block-size/worker-count sweeps.

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
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

const benchProfile1GB = 1024 * 1024 * 1024

// TestPullFileProfile profiles a 1 GB no-FUSE PullFile transfer with pprof
// CPU and heap captures. Run with -v to see throughput and profile paths.
func TestPullFileProfile(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping pull-file profile in short mode")
	}

	req := require.New(t)

	tp := SetupPeerPairWithTimeout(t, false, 600*time.Second)

	// Write 1 GB random data to Alice's save dir.
	data := make([]byte, benchProfile1GB)
	_, err := rand.Read(data)
	req.NoError(err)

	alicePath := filepath.Join(tp.AliceSaveDir, "bench_profile_1gb.bin")
	req.NoError(os.WriteFile(alicePath, data, 0644))
	req.NoError(tp.Alice.AddFile(alicePath))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, "bench_profile_1gb.bin", 30*time.Second)

	// Snapshot memory before.
	var memBefore, memAfter runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	cpuFile, err := os.CreateTemp("", "kd-cpu-*.prof")
	req.NoError(err)
	req.NoError(pprof.StartCPUProfile(cpuFile))

	destPath := filepath.Join(tp.BobSaveDir, "pulled_bench_profile_1gb.bin")
	start := time.Now()
	pullErr := tp.Bob.PullFile("bench_profile_1gb.bin", destPath)
	elapsed := time.Since(start)

	pprof.StopCPUProfile()
	cpuFile.Close()

	runtime.ReadMemStats(&memAfter)

	heapFile, err := os.CreateTemp("", "kd-heap-*.prof")
	req.NoError(err)
	req.NoError(pprof.WriteHeapProfile(heapFile))
	heapFile.Close()

	mbps := float64(benchProfile1GB) / elapsed.Seconds() / (1024 * 1024)
	t.Logf("throughput=%.1f MB/s elapsed=%s", mbps, elapsed)
	t.Logf("cpu_profile=%s", cpuFile.Name())
	t.Logf("heap_profile=%s", heapFile.Name())
	t.Logf("TotalAlloc_delta=%d bytes", memAfter.TotalAlloc-memBefore.TotalAlloc)
	t.Logf("NumGC_delta=%d", memAfter.NumGC-memBefore.NumGC)
	t.Logf("PauseTotalNs_delta=%d ns", memAfter.PauseTotalNs-memBefore.PauseTotalNs)

	req.NoError(pullErr)

	info, err := os.Stat(destPath)
	req.NoError(err)
	req.Equal(int64(benchProfile1GB), info.Size(), "pulled file size mismatch")
}

// TestSecureConnThroughput measures raw SecureConn write→read throughput
// over net.Pipe() without gRPC overhead, across three block sizes.
// A raw-net-pipe sub-test establishes a baseline without encryption.
func TestSecureConnThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SecureConn throughput in short mode")
	}

	const total = 1 << 30 // 1 GiB
	blockSizes := []int{1 << 20, 4 << 20, 16 << 20} // 1 MiB, 4 MiB, 16 MiB

	for _, bs := range blockSizes {
		bs := bs
		t.Run(fmt.Sprintf("%dMiB", bs>>20), func(t *testing.T) {
			c1, c2 := net.Pipe()

			key := make([]byte, 32)
			_, err := rand.Read(key)
			require.NoError(t, err)

			writer := session.NewSecureConn(c1, key, kbc.CipherAES256)
			reader := session.NewSecureConn(c2, key, kbc.CipherAES256)

			buf := make([]byte, bs)
			_, err = rand.Read(buf)
			require.NoError(t, err)

			start := time.Now()
			doneCh := make(chan error, 1)
			go func() {
				n := total / bs
				for i := 0; i < n; i++ {
					if _, werr := writer.Write(buf); werr != nil {
						doneCh <- werr
						return
					}
				}
				doneCh <- nil
			}()

			readBuf := make([]byte, bs)
			received := 0
			for received < total {
				n, rerr := io.ReadFull(reader, readBuf)
				received += n
				if rerr != nil {
					break
				}
			}

			elapsed := time.Since(start)
			<-doneCh
			c1.Close()
			c2.Close()

			mbps := float64(total) / elapsed.Seconds() / (1 << 20)
			t.Logf("SecureConn block=%dMiB throughput=%.1f MB/s", bs>>20, mbps)
		})
	}

	// Baseline: raw net.Pipe() without encryption.
	t.Run("raw-net-pipe", func(t *testing.T) {
		const bs = 1 << 20 // 1 MiB
		c1, c2 := net.Pipe()

		buf := make([]byte, bs)
		_, err := rand.Read(buf)
		require.NoError(t, err)

		start := time.Now()
		doneCh := make(chan error, 1)
		go func() {
			n := total / bs
			for i := 0; i < n; i++ {
				if _, werr := c1.Write(buf); werr != nil {
					doneCh <- werr
					return
				}
			}
			doneCh <- nil
		}()

		readBuf := make([]byte, bs)
		received := 0
		for received < total {
			n, rerr := io.ReadFull(c2, readBuf)
			received += n
			if rerr != nil {
				break
			}
		}

		elapsed := time.Since(start)
		<-doneCh
		c1.Close()
		c2.Close()

		mbps := float64(total) / elapsed.Seconds() / (1 << 20)
		t.Logf("raw-net-pipe block=1MiB throughput=%.1f MB/s", mbps)
	})
}

// pullFileWithParams times a PullFileWithParams call with the given blockSize and nWorkers.
func pullFileWithParams(
	_ context.Context,
	kd *common.KeibiDrop,
	remoteName, destPath string,
	blockSize, nWorkers int,
) (time.Duration, error) {
	start := time.Now()
	err := kd.PullFileWithParams(remoteName, destPath, blockSize, nWorkers)
	return time.Since(start), err
}

// TestBlockSizeSweep runs pullFileWithParams over multiple block-size values
// for a 1 GB file, three repetitions each. Because the session internals are
// unexported, block size variation is noted in output but the underlying
// transfer always uses the config default (see pullFileWithParams NOTE).
func TestBlockSizeSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping block-size sweep in short mode")
	}

	req := require.New(t)

	tp := SetupPeerPairWithTimeout(t, false, 600*time.Second)

	// Write 1 GB file once; Bob pulls it repeatedly.
	data := make([]byte, benchProfile1GB)
	_, err := rand.Read(data)
	req.NoError(err)

	alicePath := filepath.Join(tp.AliceSaveDir, "bench_sweep_1gb.bin")
	req.NoError(os.WriteFile(alicePath, data, 0644))
	req.NoError(tp.Alice.AddFile(alicePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "bench_sweep_1gb.bin", 30*time.Second)

	sizes := []int{256 << 10, 1 << 20, 4 << 20, 16 << 20} // 256 KiB, 1 MiB, 4 MiB, 16 MiB
	nWorkers := 4

	for _, blockSize := range sizes {
		blockSize := blockSize
		for rep := 0; rep < 3; rep++ {
			destPath := filepath.Join(tp.BobSaveDir,
				fmt.Sprintf("sweep_bs%d_rep%d.bin", blockSize, rep))
			// Remove prior pull so PullFile performs a fresh download.
			os.Remove(destPath)

			elapsed, pullErr := pullFileWithParams(
				context.Background(),
				tp.Bob,
				"bench_sweep_1gb.bin",
				destPath,
				blockSize,
				nWorkers,
			)
			req.NoError(pullErr)

			mbps := float64(benchProfile1GB) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("SWEEP\tblock=%dKiB\trep=%d\t%.1f MB/s",
				blockSize/1024, rep, mbps)
		}
	}
}

// TestWorkerCountSweep runs pullFileWithParams over multiple worker counts for
// a 1 GB file, three repetitions each. Because the session internals are
// unexported, worker count variation is noted in output but the underlying
// transfer always uses the config default (see pullFileWithParams NOTE).
func TestWorkerCountSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping worker-count sweep in short mode")
	}

	req := require.New(t)

	tp := SetupPeerPairWithTimeout(t, false, 600*time.Second)

	// Write 1 GB file once; Bob pulls it repeatedly.
	data := make([]byte, benchProfile1GB)
	_, err := rand.Read(data)
	req.NoError(err)

	alicePath := filepath.Join(tp.AliceSaveDir, "bench_wcsweep_1gb.bin")
	req.NoError(os.WriteFile(alicePath, data, 0644))
	req.NoError(tp.Alice.AddFile(alicePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "bench_wcsweep_1gb.bin", 30*time.Second)

	workerCounts := []int{1, 2, 4, 8, 16}
	blockSize := 1 << 20 // 1 MiB

	for _, nWorkers := range workerCounts {
		nWorkers := nWorkers
		for rep := 0; rep < 3; rep++ {
			destPath := filepath.Join(tp.BobSaveDir,
				fmt.Sprintf("sweep_wc%d_rep%d.bin", nWorkers, rep))
			os.Remove(destPath)

			elapsed, pullErr := pullFileWithParams(
				context.Background(),
				tp.Bob,
				"bench_wcsweep_1gb.bin",
				destPath,
				blockSize,
				nWorkers,
			)
			req.NoError(pullErr)

			mbps := float64(benchProfile1GB) / elapsed.Seconds() / (1024 * 1024)
			t.Logf("SWEEP\tworkers=%d\trep=%d\t%.1f MB/s",
				nWorkers, rep, mbps)
		}
	}
}
