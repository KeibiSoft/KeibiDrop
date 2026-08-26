// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: KD_PERF-gated hot-path latency storms: the swap-save rename (the
// ABOUTME: app-save pattern), cold random-offset reads, and cold-path writes.

package tests

import (
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

func skipUnlessPerf(t *testing.T) {
	t.Helper()
	if os.Getenv("KD_PERF") == "" {
		t.Skip("latency storm; set KD_PERF=1 to run")
	}
	skipIfNoFUSE(t)
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(p * float64(len(s)-1))
	return s[idx]
}

func logDist(t *testing.T, name string, ds []time.Duration) {
	t.Helper()
	t.Logf("%s: n=%d p50=%v p95=%v p99=%v max=%v", name, len(ds),
		pct(ds, 0.50).Round(time.Microsecond), pct(ds, 0.95).Round(time.Microsecond),
		pct(ds, 0.99).Round(time.Microsecond), pct(ds, 1.0).Round(time.Microsecond))
}

// TestPerfHotpath_SwapSaveStorm measures the atomic app-save through the
// mount: write a working copy, rename it over the target, 100 times. The
// rename-only distribution is the gate: no conflict fix touches the Rename
// path, so it must stay at baseline.
func TestPerfHotpath_SwapSaveStorm(t *testing.T) {
	skipUnlessPerf(t)
	require := require.New(t)

	tp := SetupFUSEPeerPair(t, 180*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	payload := make([]byte, 256*1024)
	_, err := rand.Read(payload)
	require.NoError(err)

	doc := filepath.Join(tp.AliceMountDir, "storm.bin")
	require.NoError(os.WriteFile(doc, payload, 0o644))

	const iters = 100
	renameDur := make([]time.Duration, 0, iters)
	saveDur := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		payload[0] = byte(i) // distinct content per save
		tmp := doc + ".tmp"
		t0 := time.Now()
		require.NoError(os.WriteFile(tmp, payload, 0o644))
		t1 := time.Now()
		require.NoError(os.Rename(tmp, doc))
		t2 := time.Now()
		renameDur = append(renameDur, t2.Sub(t1))
		saveDur = append(saveDur, t2.Sub(t0))
	}
	logDist(t, "swap-save rename-only", renameDur)
	logDist(t, "swap-save full (write+rename)", saveDur)
}

// TestPerfHotpath_ColdRandomAccess measures random jumps into cold regions
// of a large remote file (prefetch off, pure on-demand) and writes landing
// on cold paths. First-byte latency per jump.
func TestPerfHotpath_ColdRandomAccess(t *testing.T) {
	skipUnlessPerf(t)
	require := require.New(t)

	t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")

	sizeMB := 64
	if v := os.Getenv("KD_PERF_SIZE_MB"); v != "" {
		fmt.Sscanf(v, "%d", &sizeMB) //nolint:errcheck // best-effort override
	}

	tp := SetupFUSEPeerPairPreConnect(t, 300*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			big := make([]byte, sizeMB*1024*1024)
			_, err := rand.Read(big)
			require.NoError(err)
			require.NoError(os.WriteFile(filepath.Join(bobSave, "cold.bin"), big, 0o644))
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	coldPath := filepath.Join(tp.AliceMountDir, "cold.bin")
	WaitForFileOnMount(t, coldPath, 60*time.Second)

	fh, err := os.Open(coldPath) // #nosec G304
	require.NoError(err)
	defer fh.Close()
	info, err := fh.Stat()
	require.NoError(err)
	size := info.Size()

	rng := mrand.New(mrand.NewSource(42)) // #nosec G404 -- deterministic bench
	const jumps = 32
	buf := make([]byte, 4096)
	readDur := make([]time.Duration, 0, jumps)
	for i := 0; i < jumps; i++ {
		off := rng.Int63n(size - int64(len(buf)))
		t0 := time.Now()
		_, err := fh.ReadAt(buf, off)
		require.NoError(err)
		readDur = append(readDur, time.Since(t0))
	}
	logDist(t, fmt.Sprintf("cold random 4K reads over %d MiB", sizeMB), readDur)

	// Cold-path writes: 4K writes at random offsets into the still-cold
	// file through the mount (writes landing in unfetched regions).
	wfh, err := os.OpenFile(coldPath, os.O_WRONLY, 0o644) // #nosec G304
	require.NoError(err)
	defer wfh.Close()
	writeDur := make([]time.Duration, 0, jumps)
	for i := 0; i < jumps; i++ {
		off := rng.Int63n(size - int64(len(buf)))
		t0 := time.Now()
		_, err := wfh.WriteAt(buf, off)
		require.NoError(err)
		writeDur = append(writeDur, time.Since(t0))
	}
	logDist(t, "cold-path random 4K writes", writeDur)
}
