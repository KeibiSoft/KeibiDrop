// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: The mount must serve the ORIGIN's times for un-edited remote files
// ABOUTME: before, during, and after a cold fetch. The demo-day custody bug.

package tests

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// metadataViewCase runs the backdated-origin matrix for one preserve_metadata
// setting. The view must serve origin times at every stage on every platform;
// the disk copy carries origin times only when the flag is on.
func metadataViewCase(t *testing.T, preserve bool) {
	skipIfNoFUSE(t)
	require := require.New(t)

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	content := makeTestPattern(61 * 1024)

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			dir := filepath.Join(bobSave, "config")
			require.NoError(os.MkdirAll(dir, 0o755))
			p := filepath.Join(dir, "SYSTEM")
			require.NoError(os.WriteFile(p, content, 0o644))
			require.NoError(os.Chmod(p, 0o640))
			require.NoError(os.Chtimes(p, origAtime, origMtime))
			bob.ScanSharedOnStart = true
			bob.ShareReadOnly = true
			alice.PreserveMetadata = preserve
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	mountPath := filepath.Join(tp.AliceMountDir, "config", "SYSTEM")
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(mountPath)
		return err == nil && info.Size() == int64(len(content))
	}, "waiting for SYSTEM to be announced")

	assertView := func(stage string) {
		info, err := os.Stat(mountPath)
		require.NoError(err, stage)
		require.WithinDuration(origMtime, info.ModTime(), time.Second,
			"%s: the mount must serve the ORIGIN mtime, got %v", stage, info.ModTime())
		if runtime.GOOS != "windows" {
			require.Equal(os.FileMode(0o640), info.Mode().Perm(),
				"%s: the mount must serve the ORIGIN mode", stage)
		}
	}

	// (a) Before any read: pure announce view.
	assertView("before read")

	// (b) Cold read with the handle still open: the cache file now exists
	// with a fresh mtime. This is the exact window robocopy sampled.
	fh, err := os.Open(mountPath)
	require.NoError(err)
	got, err := io.ReadAll(fh)
	require.NoError(err)
	require.Equal(content, got, "bytes must be exact")
	assertView("during open, after cold fetch")
	require.NoError(fh.Close())

	// (c) After release.
	assertView("after release")

	// A copy out of the mount now carries the origin mtime, whatever tool
	// made it, because it stamps from the view.
	diskCopy := filepath.Join(t.TempDir(), "SYSTEM")
	require.NoError(copyFileByRead(mountPath, diskCopy))
	viewInfo, err := os.Stat(mountPath)
	require.NoError(err)
	require.NoError(os.Chtimes(diskCopy, time.Time{}, viewInfo.ModTime()))
	copyInfo, err := os.Stat(diskCopy)
	require.NoError(err)
	require.WithinDuration(origMtime, copyInfo.ModTime(), time.Second,
		"a view-stamped copy must carry the origin mtime")

	// The saved cache copy: origin times exactly when the flag is on.
	savedPath := filepath.Join(tp.AliceSaveDir, "config", "SYSTEM")
	if preserve {
		WaitForCondition(t, 15*time.Second, 200*time.Millisecond, func() bool {
			info, err := os.Stat(savedPath)
			if err != nil {
				return false
			}
			d := info.ModTime().Sub(origMtime)
			return d.Abs() <= time.Second
		}, "waiting for preserve_metadata to stamp the saved copy")
	} else {
		info, err := os.Stat(savedPath)
		require.NoError(err)
		require.Greater(info.ModTime().Sub(origMtime).Abs(), time.Minute,
			"flag off: the saved copy keeps its write time")
	}
}

func copyFileByRead(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}

func TestMetadataView_OriginTimesServed_PreserveOn(t *testing.T) {
	metadataViewCase(t, true)
}

func TestMetadataView_OriginTimesServed_PreserveOff(t *testing.T) {
	metadataViewCase(t, false)
}

// TestMetadataView_LocalEditWinsTheView: once the analyst genuinely edits a
// file (writable mount), the view follows the local edit again, not the
// origin. The guard must never hide real local changes.
func TestMetadataView_LocalEditWinsTheView(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	content := makeTestPattern(8 * 1024)

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			p := filepath.Join(bobSave, "notes.txt")
			require.NoError(os.WriteFile(p, content, 0o644))
			require.NoError(os.Chtimes(p, origMtime, origMtime))
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	mountPath := filepath.Join(tp.AliceMountDir, "notes.txt")
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(mountPath)
		return err == nil && info.Size() == int64(len(content))
	}, "waiting for notes.txt")

	// Cold read, then a real local edit through the mount.
	_, err := os.ReadFile(mountPath)
	require.NoError(err)
	require.NoError(os.WriteFile(mountPath, []byte("locally edited"), 0o644))

	WaitForCondition(t, 10*time.Second, 100*time.Millisecond, func() bool {
		info, err := os.Stat(mountPath)
		return err == nil && info.ModTime().After(time.Now().Add(-1*time.Minute))
	}, "after a local edit the view must show the fresh local mtime")
}
