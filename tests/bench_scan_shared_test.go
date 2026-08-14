// ABOUTME: Macro benchmark for ScanAndShareSaveDir — 1k and 10k pre-existing files.
// ABOUTME: Records wall-clock and bytes-on-wire; proves announce is metadata-only.

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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestScanShareBenchmark measures the populated-folder startup scan: wall-clock
// to announce N pre-existing files and bytes-on-wire for the announce burst.
// The content must NOT move (metadata only), so wire bytes stay far below the
// seeded content size. Skipped in short mode.
func TestScanShareBenchmark(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping scan benchmark in short mode")
	}

	const fileSize = 4096

	for _, count := range []int{1000, 10000} {
		count := count
		t.Run(fmt.Sprintf("%dfiles", count), func(t *testing.T) {
			require := require.New(t)
			tp := SetupPeerPairWithTimeout(t, false, 600*time.Second)

			// Seed AFTER connect with the flag off, then invoke the scan
			// directly: same code path the flag runs, but the timer excludes
			// pairing. Spread across subdirs to exercise the BFS walk.
			payload := make([]byte, fileSize)
			_, err := rand.Read(payload)
			require.NoError(err)
			perDir := 100
			for i := 0; i < count; i++ {
				dir := filepath.Join(tp.BobSaveDir, fmt.Sprintf("d%03d", i/perDir))
				if i%perDir == 0 {
					require.NoError(os.MkdirAll(dir, 0o755))
				}
				require.NoError(os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d.bin", i)), payload, 0o644))
			}

			sentBefore, _ := tp.Bob.WireStats()

			start := time.Now()
			n, err := tp.Bob.ScanAndShareSaveDir(context.Background())
			scanDur := time.Since(start)
			require.NoError(err)
			require.Equal(count, n)

			// Until the peer tracks every file (includes peer-side processing).
			WaitForCondition(t, 300*time.Second, 100*time.Millisecond, func() bool {
				tp.Alice.SyncTracker.RemoteFilesMu.RLock()
				defer tp.Alice.SyncTracker.RemoteFilesMu.RUnlock()
				return len(tp.Alice.SyncTracker.RemoteFiles) >= count
			}, "waiting for peer to track all announced files")
			e2eDur := time.Since(start)

			sentAfter, _ := tp.Bob.WireStats()
			wireBytes := sentAfter - sentBefore
			contentBytes := uint64(count) * fileSize

			t.Logf("scan_share_bench files=%d file_size=%d scan_wall=%s e2e_wall=%s files_per_sec=%.0f wire_bytes=%d wire_per_file=%d content_bytes=%d wire_vs_content=%.1f%%",
				count, fileSize, scanDur, e2eDur,
				float64(count)/e2eDur.Seconds(), wireBytes, wireBytes/uint64(count),
				contentBytes, 100*float64(wireBytes)/float64(contentBytes))

			// Metadata-only claim: the announce burst must not move content.
			require.Less(wireBytes, contentBytes/2,
				"announce moved too many bytes; content should stream on demand only")
		})
	}
}
