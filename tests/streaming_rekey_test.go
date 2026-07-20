// ABOUTME: Streaming-under-rekey latency tests: a large media file streamed while the always-on
// ABOUTME: ratchet bumps key-epochs must not stall reads or stutter paced playback.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
)

// durPercentile returns the p-quantile of ds (p in [0,1]); 0 for an empty slice.
func durPercentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s)-1) * p)
	return s[idx]
}

// forceFrequentRatchet drops the in-band ratchet byte threshold so a long transfer
// crosses many key-epochs. Assigning the var directly bypasses the env-override floor
// (minRekeyBytes); restored on cleanup after the pair is torn down (registered before
// SetupFUSEPeerPair, so its cleanup runs last), so no writer reads it concurrently.
func forceFrequentRatchet(t *testing.T, bytesPerBump uint64) {
	t.Helper()
	origBytes, origMsgs := session.RekeyBytesThreshold, session.RekeyMsgsThreshold
	session.RekeyBytesThreshold = bytesPerBump
	t.Cleanup(func() {
		session.RekeyBytesThreshold, session.RekeyMsgsThreshold = origBytes, origMsgs
	})
}

// maxWriterEpoch reads the higher of the two peers' live writer epochs. The peer
// serving file data ratchets its writer, but taking the max catches a bump either way.
func maxWriterEpoch(tp *TestPair) func() uint16 {
	return func() uint16 {
		a := tp.Alice.HealthMonitor.MaxWriterEpoch()
		b := tp.Bob.HealthMonitor.MaxWriterEpoch()
		if a > b {
			return a
		}
		return b
	}
}

// capturingProvider records the (inode, path) of the first opened remote file, so a
// test can then open its own fine-grained stream to the same file. Otherwise delegates.
type capturingProvider struct {
	inner  types.FileStreamProvider
	onOpen func(inode uint64, path string)
}

func (p *capturingProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	p.onOpen(inode, path)
	return p.inner.OpenRemoteFile(ctx, inode, path)
}

func (p *capturingProvider) StreamFile(ctx context.Context, path string, startOffset uint64) (types.StreamFileReceiver, error) {
	return p.inner.StreamFile(ctx, path, startOffset)
}

// TestStream_RekeyDuringPlaybackAddsNoStall streams a 64 MiB "movie" from the serving
// peer in 256 KiB reads (one gRPC round trip each, the granularity a player consumes)
// while the always-on symmetric ratchet is forced to bump its key-epoch every ~1 MiB. It
// isolates the rekey's cost: reads that coincide with an epoch bump are compared head to
// head with reads that don't. The in-band ratchet is a local ~microsecond HKDF folded
// into the next record (make-before-break, no round trip), so the two buckets must match
// and no read may stall long enough to starve a player's jitter buffer.
func TestStream_RekeyDuringPlaybackAddsNoStall(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping streaming-under-rekey in short mode")
	}

	// Pure on-demand (no eager prefetch on open) and one epoch bump per ~1 MiB served.
	// With 256 KiB wire reads the serving writer flushes 256 KiB per response, so it
	// ratchets once every ~4 reads: a natural mix of bumped and clean reads.
	t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")
	forceFrequentRatchet(t, 1<<20)

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	requireResilienceReady(t, tp)
	require := require.New(t)
	getEpoch := maxWriterEpoch(tp)

	// Capture the exact (inode, path) the FUSE layer uses, so our own stream addresses
	// the same remote file without guessing the wire path format.
	var capMu sync.Mutex
	var capInode uint64
	var capPath string
	var capOK bool
	origFactory := tp.Alice.FS.Root.OpenStreamProvider
	tp.Alice.FS.Root.OpenStreamProvider = func() types.FileStreamProvider {
		return &capturingProvider{inner: origFactory(), onOpen: func(inode uint64, path string) {
			capMu.Lock()
			if !capOK {
				capInode, capPath, capOK = inode, path, true
			}
			capMu.Unlock()
		}}
	}

	const movieSize = 64 << 20
	movie := make([]byte, movieSize)
	_, err := rand.Read(movie)
	require.NoError(err)
	bobPath := filepath.Join(tp.BobSaveDir, "movie.bin")
	require.NoError(os.WriteFile(bobPath, movie, 0644))
	require.NoError(tp.Bob.AddFile(bobPath))

	alicePath := filepath.Join(tp.AliceMountDir, "movie.bin")
	WaitForFileOnMount(t, alicePath, 30*time.Second)

	// One small FUSE read triggers OpenRemoteFile so we capture (inode, path).
	f0, err := os.Open(alicePath)
	require.NoError(err)
	_, _ = f0.Read(make([]byte, 4096))
	f0.Close() //nolint:errcheck // capture handle
	capMu.Lock()
	inode, path, ok := capInode, capPath, capOK
	capMu.Unlock()
	require.Truef(ok, "did not capture remote (inode, path)")

	// Stream the movie ourselves: 256 KiB reads, one round trip each, straight to the
	// serving peer (fresh stream bypasses Alice's cache, so every read makes Bob serve
	// and ratchet). Record each read's latency and whether the epoch advanced during it.
	ctx := context.Background()
	prov := origFactory()
	rs, err := prov.OpenRemoteFile(ctx, inode, path)
	require.NoError(err)
	defer rs.Close() //nolint:errcheck // read-only stream

	const chunk = 256 << 10
	var bumpDur, cleanDur []time.Duration
	var maxAll time.Duration
	epochStart := getEpoch()
	for off := int64(0); off < movieSize; off += chunk {
		want := int64(chunk)
		if off+want > movieSize {
			want = movieSize - off
		}
		before := getEpoch()
		start := time.Now()
		data, rerr := rs.ReadAt(ctx, off, want)
		dur := time.Since(start)
		require.NoErrorf(rerr, "read at offset %d", off)
		require.Truef(bytes.Equal(movie[off:off+int64(len(data))], data), "stream corruption at offset %d", off)
		if dur > maxAll {
			maxAll = dur
		}
		if getEpoch() != before {
			bumpDur = append(bumpDur, dur)
		} else {
			cleanDur = append(cleanDur, dur)
		}
	}
	bumps := int(getEpoch()) - int(epochStart)

	require.GreaterOrEqualf(bumps, 10, "expected many epoch bumps during the stream, got %d", bumps)
	require.NotEmpty(bumpDur, "no read coincided with a rekey")
	require.NotEmpty(cleanDur, "no rekey-free read to use as a control group")

	bumpP50, bumpP99 := durPercentile(bumpDur, 0.50), durPercentile(bumpDur, 0.99)
	cleanP50, cleanP99 := durPercentile(cleanDur, 0.50), durPercentile(cleanDur, 0.99)
	t.Logf("stream: %d reads (%d rekey-coincident, %d clean), %d epoch bumps over %d MiB",
		len(bumpDur)+len(cleanDur), len(bumpDur), len(cleanDur), bumps, movieSize>>20)
	t.Logf("  rekey-coincident: p50=%s p99=%s", bumpP50, bumpP99)
	t.Logf("  clean:            p50=%s p99=%s", cleanP50, cleanP99)
	t.Logf("  slowest read overall: %s", maxAll)

	// No read may stall long enough to starve a player's jitter buffer.
	require.Lessf(maxAll, 2*time.Second, "a read stalled %s (buffer-starving)", maxAll)

	// The heart of it: the median rekey-coincident read must not be materially slower
	// than the median clean read. The ratchet is a local microsecond KDF, so the medians
	// match; a blocking rekey (a round trip or a lock wait) would shift the bumped median.
	require.Lessf(bumpP50, cleanP50+5*time.Millisecond,
		"rekey-coincident reads are slower: bumpP50=%s vs cleanP50=%s", bumpP50, cleanP50)
}

// TestStream_PacedBluRayPlaybackNoRekeyUnderrun is the literal Blu-ray simulation: a
// 32 MiB clip consumed at a sustained 40 Mbps in 256 KiB blocks (a player draining its
// jitter buffer at playback rate) while the ratchet rekeys throughout. It asserts no single
// block read outlasts a 2 s jitter buffer, i.e. playback never stutters because of a rekey.
// Reads are paced to playback speed so a rekey-induced stall would eat the buffer rather than
// be masked by reading far ahead.
func TestStream_PacedBluRayPlaybackNoRekeyUnderrun(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("skipping paced Blu-ray playback in short mode")
	}

	t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")
	forceFrequentRatchet(t, 1<<20)

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	requireResilienceReady(t, tp)
	require := require.New(t)
	getEpoch := maxWriterEpoch(tp)

	const clipSize = 32 << 20            // 32 MiB clip
	const block = 256 << 10              // 256 KiB read block
	const bitrateBytesPerSec = 40e6 / 8  // 40 Mbps Blu-ray video ~= 5 MB/s
	const jitterBuffer = 2 * time.Second // player pre-buffers ~2 s; a longer fetch stutters
	blockInterval := time.Duration(float64(block) / float64(bitrateBytesPerSec) * float64(time.Second))

	clip := make([]byte, clipSize)
	_, err := rand.Read(clip)
	require.NoError(err)
	bobPath := filepath.Join(tp.BobSaveDir, "clip.bin")
	require.NoError(os.WriteFile(bobPath, clip, 0644))
	require.NoError(tp.Bob.AddFile(bobPath))

	alicePath := filepath.Join(tp.AliceMountDir, "clip.bin")
	WaitForFileOnMount(t, alicePath, 30*time.Second)

	f, err := os.Open(alicePath)
	require.NoError(err)
	defer f.Close() //nolint:errcheck // read-only handle

	epochStart := getEpoch()
	buf := make([]byte, block)
	var maxFetch time.Duration
	var slowestOffset int64
	var offset int64
	for {
		readStart := time.Now()
		n, rerr := io.ReadFull(f, buf)
		fetch := time.Since(readStart)
		if n > 0 {
			require.Truef(bytes.Equal(buf[:n], clip[offset:offset+int64(n)]),
				"stream corruption at offset %d", offset)
			if fetch > maxFetch {
				maxFetch, slowestOffset = fetch, offset
			}
			require.Lessf(fetch, jitterBuffer,
				"fetch at offset %d took %s, longer than the %s jitter buffer (playback stutter)",
				offset, fetch, jitterBuffer)
			offset += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		require.NoError(rerr)
		// Pace to playback rate: if the fetch beat the block's playback time, wait out
		// the difference (buffer full). A stalled fetch skips the sleep and drains buffer.
		if fetch < blockInterval {
			time.Sleep(blockInterval - fetch)
		}
	}

	bumps := int(getEpoch()) - int(epochStart)
	require.Equal(int64(clipSize), offset, "did not stream the whole clip")
	require.GreaterOrEqualf(bumps, 5, "expected rekeys during playback, got %d", bumps)
	t.Logf("paced Blu-ray playback: %d MiB @ 40 Mbps, %d rekeys, no underrun; slowest fetch %s at offset %d (buffer=%s)",
		clipSize>>20, bumps, maxFetch.Round(time.Millisecond), slowestOffset, jitterBuffer)
}

// measureBulkThroughput pulls a `size`-byte file Bob->Alice over loopback `iters` times and
// returns the MEDIAN MB/s (robust to a single slow pull) and the total writer epoch bumps. A
// fresh pair is used so the caller can set the ratchet threshold before it connects (race-free).
func measureBulkThroughput(t *testing.T, size, iters int, forceRekey bool) (float64, int) {
	t.Helper()
	if forceRekey {
		forceFrequentRatchet(t, 1<<20)
	}
	tp := SetupPeerPair(t, false)
	requireResilienceReady(t, tp)
	require := require.New(t)
	getEpoch := maxWriterEpoch(tp)

	payload := make([]byte, size)
	_, err := rand.Read(payload)
	require.NoError(err)

	e0 := getEpoch()
	rates := make([]float64, 0, iters)
	for i := 0; i < iters; i++ {
		name := fmt.Sprintf("bulk_%d.bin", i)
		src := filepath.Join(tp.BobSaveDir, name)
		require.NoError(os.WriteFile(src, payload, 0644))
		require.NoError(tp.Bob.AddFile(src))
		WaitForRemoteFile(t, tp.Alice.SyncTracker, name, 30*time.Second)
		dst := filepath.Join(tp.AliceSaveDir, name)
		start := time.Now()
		require.NoError(tp.Alice.PullFile(name, dst))
		rates = append(rates, float64(size)/time.Since(start).Seconds()/(1<<20))
		got, err := os.ReadFile(dst)
		require.NoError(err)
		require.Truef(bytes.Equal(payload, got), "bulk payload mismatch on %s", name)
	}
	bumps := int(getEpoch()) - int(e0)
	sort.Float64s(rates)
	return rates[len(rates)/2], bumps // median
}

// TestStream_BulkThroughputUnaffectedByRekey answers, deterministically and without network
// noise, whether forcing the ratchet to rotate every ~1 MiB dents sustained transfer throughput.
// It pulls 128 MiB over loopback with the ratchet at its production threshold (zero rotations at
// this size) versus forced to rotate ~32 times, on separate pairs so the threshold is set before
// connect. The ratchet is a local microsecond KDF folded into the writer (no round trip, no flush
// forced), so the forced-rekey throughput must stay within noise of the baseline. This is the
// deterministic counterpart to the noisy single-trial WAN off-vs-on comparison.
func TestStream_BulkThroughputUnaffectedByRekey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping bulk-throughput-under-rekey in short mode")
	}
	const size = 128 << 20
	const iters = 3 // median of 3 pulls per phase, robust to a single slow pull

	var offMBps, onMBps float64
	var onBumps int
	t.Run("rekey-off", func(t *testing.T) { offMBps, _ = measureBulkThroughput(t, size, iters, false) })
	t.Run("rekey-on", func(t *testing.T) { onMBps, onBumps = measureBulkThroughput(t, size, iters, true) })

	t.Logf("local bulk %d MiB (median of %d): rekey-off %.0f MB/s, rekey-on %.0f MB/s (%d ratchets), ratio on/off=%.2f",
		size>>20, iters, offMBps, onMBps, onBumps, onMBps/offMBps)
	require.GreaterOrEqualf(t, onBumps, 10, "rekey-on must actually rotate, got %d", onBumps)
	// The ratchet KDF is negligible; on loopback the forced-rekey median must stay within 20% of
	// baseline. A real per-rotation stall or flush cost would blow past this.
	require.Greaterf(t, onMBps, offMBps*0.8,
		"forced rekey cut bulk throughput: off=%.0f on=%.0f MB/s", offMBps, onMBps)
}
