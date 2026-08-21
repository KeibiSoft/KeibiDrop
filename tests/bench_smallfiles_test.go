// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Small-file extraction walk benchmark, KAPE-shaped: many small
// ABOUTME: files read sequentially through the mount, warm on vs off.

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestSmallFilesWalk measures a sequential read of every small file in a
// shared tree through the FUSE mount, the robocopy/extraction shape that was
// latency-bound in the field. Two passes: sibling warming on (default) and
// off (KEIBIDROP_WARM_SIBLINGS=0). Wall time and wire bytes are logged with
// the BENCH prefix; hard asserts cover only invariants.
func TestSmallFilesWalk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping small-files walk benchmark in short mode")
	}
	skipIfNoFUSE(t)

	count := 400
	if v, err := strconv.Atoi(os.Getenv("KD_SMALLFILES_COUNT")); err == nil && v >= 50 {
		count = v
	}
	const smallSize = 15 * 1024
	const hiveSize = 10 * 1024 * 1024

	for _, warm := range []string{"1", "0"} {
		t.Run("warm="+warm, func(t *testing.T) {
			require := require.New(t)
			t.Setenv("KEIBIDROP_WARM_SIBLINGS", warm)
			t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")

			content := make(map[string][]byte)
			var names []string
			for i := range count {
				name := fmt.Sprintf("pf_%04d.pf", i)
				names = append(names, name)
				content[name] = makeTestPattern(smallSize + i%37)
			}
			sort.Strings(names)

			tp := SetupFUSEPeerPairPreConnect(t, 600*time.Second,
				func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
					dir := filepath.Join(bobSave, "triage")
					require.NoError(os.MkdirAll(dir, 0o755))
					for name, data := range content {
						require.NoError(os.WriteFile(filepath.Join(dir, name), data, 0o644))
					}
					for h := range 3 {
						require.NoError(os.WriteFile(
							filepath.Join(dir, fmt.Sprintf("hive_%d.dat", h)),
							makeTestPattern(hiveSize), 0o644))
					}
					bob.ScanSharedOnStart = true
				})
			waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

			// All announces in before the timed walk.
			root := tp.Alice.FS.Root()
			require.NotNil(root)
			WaitForCondition(t, 120*time.Second, 200*time.Millisecond, func() bool {
				root.RemoteFilesLock.RLock()
				defer root.RemoteFilesLock.RUnlock()
				for _, name := range names {
					if root.RemoteFiles["/triage/"+name] == nil {
						return false
					}
				}
				return root.RemoteFiles["/triage/hive_2.dat"] != nil
			}, "waiting for the scan announce")

			_, recvBefore := tp.Alice.WireStats()

			start := time.Now()
			for _, name := range names {
				got, err := os.ReadFile(filepath.Join(tp.AliceMountDir, "triage", name))
				require.NoError(err, name)
				require.Equal(len(content[name]), len(got), "size mismatch: %s", name)
			}
			wall := time.Since(start)

			// Byte-exactness spot check across the walk.
			for _, name := range []string{names[0], names[len(names)/2], names[len(names)-1]} {
				got, err := os.ReadFile(filepath.Join(tp.AliceMountDir, "triage", name))
				require.NoError(err)
				require.Equal(content[name], got, "bytes differ: %s", name)
			}

			_, recvAfter := tp.Alice.WireStats()
			recvDelta := recvAfter - recvBefore
			var payload uint64
			for _, name := range names {
				payload += uint64(len(content[name]))
			}

			// Invariants: one copy of the payload moved, not two; the hives
			// never warm.
			require.GreaterOrEqual(recvDelta, payload*9/10, "files did not travel the wire")
			require.Less(recvDelta, payload*3/2+256*1024, "duplicate fetching on the wire")
			root.RemoteFilesLock.RLock()
			hive := root.RemoteFiles["/triage/hive_0.dat"]
			root.RemoteFilesLock.RUnlock()
			require.NotNil(hive)
			require.False(hive.Bitmap.IsComplete(), "hive must not be warmed")

			filesPerSec := float64(count) / wall.Seconds()
			t.Logf("BENCH smallfiles_walk warm=%s files=%d bytes=%d wall=%s files_per_sec=%.1f wire_recv=%d wire_vs_payload=%.2f",
				warm, count, payload, wall, filesPerSec, recvDelta, float64(recvDelta)/float64(payload))
		})
	}
}
