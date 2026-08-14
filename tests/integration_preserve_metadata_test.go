// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration tests for preserve_metadata — a file's original
// ABOUTME: mtime, atime and mode survive the transfer onto the receiver's disk.

package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestPreserveMetadata_FUSEReceiverAppliesOriginTimes is the DFIR scenario:
// evidence collected days ago moves to an analyst's machine, and the bytes
// saved on the analyst's disk keep the original mtime and mode. Exercises the
// FUSE Release completion path (read-through download).
func TestPreserveMetadata_FUSEReceiverAppliesOriginTimes(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	content := []byte("evidence: original timestamps must survive the transfer")

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			p := filepath.Join(bobSave, "evidence.bin")
			require.NoError(os.WriteFile(p, content, 0o644))
			require.NoError(os.Chmod(p, 0o640))
			require.NoError(os.Chtimes(p, origAtime, origMtime))
			bob.ScanSharedOnStart = true
			alice.PreserveMetadata = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	mountPath := filepath.Join(tp.AliceMountDir, "evidence.bin")
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(mountPath)
		return err == nil && info.Size() == int64(len(content))
	}, "waiting for evidence.bin on Alice's mount")

	// Full read: the bitmap completes and Release applies the origin metadata.
	got, err := os.ReadFile(mountPath)
	require.NoError(err)
	require.Equal(content, got)

	diskPath := filepath.Join(tp.AliceSaveDir, "evidence.bin")
	WaitForCondition(t, 15*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(diskPath)
		if err != nil {
			return false
		}
		d := info.ModTime().Sub(origMtime)
		if d < 0 {
			d = -d
		}
		return d <= time.Second
	}, "waiting for the origin mtime on Alice's saved bytes")

	info, err := os.Stat(diskPath)
	require.NoError(err)
	require.WithinDuration(origMtime, info.ModTime(), time.Second, "disk mtime must be the origin's")
	if runtime.GOOS != "windows" {
		require.Equal(os.FileMode(0o640), info.Mode().Perm(), "disk mode must be the origin's")
	}

	// The origin's real atime crossed the wire and fed the apply (one Chtimes
	// call sets atime and mtime together; the mtime assert above proves it ran).
	root := tp.Alice.FS.Root()
	root.RemoteFilesLock.RLock()
	f := root.RemoteFiles["/evidence.bin"]
	root.RemoteFilesLock.RUnlock()
	require.NotNil(f)
	require.Equal(origAtime.UnixNano(), f.OriginAtimeNs, "announce must carry the real atime")
}

// TestPreserveMetadata_PullFileAppliesOriginTimes covers the no-FUSE receiver:
// PullFile lands the bytes, finalizeDownload applies the origin metadata.
// Also the control: with the flag off, the pulled copy keeps fresh local times.
func TestPreserveMetadata_PullFileAppliesOriginTimes(t *testing.T) {
	require := require.New(t)
	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	content := []byte("pulled evidence with original metadata")
	src := filepath.Join(tp.AliceSaveDir, "evidence.bin")
	require.NoError(os.WriteFile(src, content, 0o644))
	require.NoError(os.Chmod(src, 0o640))
	require.NoError(os.Chtimes(src, origAtime, origMtime))
	require.NoError(tp.Alice.AddFile(src))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, "evidence.bin", 5*time.Second)

	// The announce carried the real atime, not an mtime copy.
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	rf := tp.Bob.SyncTracker.RemoteFiles["evidence.bin"]
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	require.NotNil(rf)
	require.Equal(uint64(origAtime.UnixNano()), rf.Atime, "tracker must hold the announced atime")

	tp.Bob.PreserveMetadata = true
	dest := filepath.Join(t.TempDir(), "evidence.bin")
	require.NoError(tp.Bob.PullFile("evidence.bin", dest))

	pulled, err := os.ReadFile(dest)
	require.NoError(err)
	require.Equal(content, pulled)
	info, err := os.Stat(dest)
	require.NoError(err)
	require.WithinDuration(origMtime, info.ModTime(), time.Second, "pulled file keeps the origin mtime")
	if runtime.GOOS != "windows" {
		require.Equal(os.FileMode(0o640), info.Mode().Perm(), "pulled file keeps the origin mode")
	}

	// Control: flag off, the local write time stays.
	tp.Bob.PreserveMetadata = false
	dest2 := filepath.Join(t.TempDir(), "evidence2.bin")
	require.NoError(tp.Bob.PullFile("evidence.bin", dest2))
	info2, err := os.Stat(dest2)
	require.NoError(err)
	require.WithinDuration(time.Now(), info2.ModTime(), time.Minute, "flag off: pulled file keeps local times")
}
