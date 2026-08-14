// ABOUTME: Region-diff verification benchmark — in-place edit of a large cached
// ABOUTME: file must resync ~the changed region, not the whole file.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"crypto/rand"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRegionDiffEditResync measures bytes-on-wire when a small region of a
// large, fully cached file changes in place. The edit-reconcile path
// (GetChunkHashes + bitmap re-mark) must keep the resync near the changed
// region size. rsync over sshfs moves the whole file on this edit (default
// whole-file mode on what it sees as local paths). Skipped in short mode.
func TestRegionDiffEditResync(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping region-diff benchmark in short mode")
	}
	skipIfNoFUSE(t)
	require := require.New(t)

	// Pure on-demand: prefetch-on-open would re-stream the whole file after
	// the edit reset and mask the reconcile win.
	t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")
	// Same-size in-place edits reach a macOS mount only with auto_cache
	// (live_collab). Without it the kernel page cache serves the pre-edit
	// bytes, the documented git-safe default.
	t.Setenv("KEIBIDROP_LIVE_COLLAB", "1")

	// KD_REGIONDIFF_MB scales the file for manual artefact runs (default 256).
	fileMB := 256
	if v, err := strconv.Atoi(os.Getenv("KD_REGIONDIFF_MB")); err == nil && v >= 64 {
		fileMB = v
	}
	var (
		fileSize = fileMB * 1024 * 1024
		editSize = 2 * 1024 * 1024
		editOff  = fileSize/2 + 12345 // unaligned on purpose
	)

	tp := SetupFUSEPeerPair(t, 600*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// Bob (origin, no-FUSE) shares a large file.
	data := make([]byte, fileSize)
	_, err := rand.Read(data)
	require.NoError(err)
	bobPath := filepath.Join(tp.BobSaveDir, "vmimage.bin")
	require.NoError(os.WriteFile(bobPath, data, 0o644))
	require.NoError(tp.Bob.AddFile(bobPath))

	alicePath := filepath.Join(tp.AliceMountDir, "vmimage.bin")
	WaitForCondition(t, 60*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(alicePath)
		return err == nil && info.Size() == int64(fileSize)
	}, "waiting for vmimage.bin to appear on Alice's mount at full size")

	// Alice caches the whole file through the mount.
	_, recvBefore := tp.Alice.WireStats()
	start := time.Now()
	sumInitial := mountFileSha256(t, alicePath)
	initialDur := time.Since(start)
	require.Equal(sha256.Sum256(data), sumInitial, "initial read must be byte-exact")
	_, recvAfterInitial := tp.Alice.WireStats()
	initialWire := recvAfterInitial - recvBefore

	// Bob edits a small region IN PLACE (no truncate, no rename) and
	// re-announces: same size, newer mtime = the in-place edit signal.
	edit := make([]byte, editSize)
	_, err = rand.Read(edit)
	require.NoError(err)
	copy(data[editOff:editOff+editSize], edit)
	f, err := os.OpenFile(bobPath, os.O_WRONLY, 0)
	require.NoError(err)
	_, err = f.WriteAt(edit, int64(editOff))
	require.NoError(err)
	require.NoError(f.Close())
	require.NoError(tp.Bob.AddFile(bobPath))

	// Wait for the announce + async reconcile traffic to settle.
	var settled uint64
	WaitForCondition(t, 60*time.Second, 250*time.Millisecond, func() bool {
		_, now := tp.Alice.WireStats()
		if now == settled {
			return true
		}
		settled = now
		return false
	}, "waiting for edit announce and reconcile traffic to settle")

	// Alice re-reads the whole file. Unchanged chunks must come from cache.
	start = time.Now()
	sumAfter := mountFileSha256(t, alicePath)
	rereadDur := time.Since(start)
	require.Equal(sha256.Sum256(data), sumAfter, "post-edit read must be byte-exact")

	_, recvFinal := tp.Alice.WireStats()
	editWire := recvFinal - recvAfterInitial

	t.Logf("regiondiff_bench file=%dMB edit=%dMB initial_wire=%dMB initial_dur=%s edit_wire_bytes=%d edit_wire_mb=%.2f reread_dur=%s edit_vs_file=%.2f%% edit_vs_region=%.1fx",
		fileSize/(1024*1024), editSize/(1024*1024),
		initialWire/(1024*1024), initialDur, editWire,
		float64(editWire)/(1024*1024), rereadDur,
		100*float64(editWire)/float64(fileSize),
		float64(editWire)/float64(editSize))

	// Sanity: the initial sync moved about the whole file.
	require.Greater(initialWire, uint64(fileSize)*9/10, "initial sync should stream the file")
	// The claim under test: the resync moves ~the region, not the file.
	require.Less(editWire, uint64(fileSize)/4,
		"edit resync moved %.1f%% of the file; region-diff did not fire", 100*float64(editWire)/float64(fileSize))
}

// mountFileSha256 reads the file through the mount in 4 MiB steps.
func mountFileSha256(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4*1024*1024)
	_, err = io.CopyBuffer(h, f, buf)
	require.NoError(t, err)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
