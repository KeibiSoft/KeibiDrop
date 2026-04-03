// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"crypto/md5"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSE_LargeFileNotificationSize verifies that when Alice writes a large
// file to her FUSE mount, Bob receives the correct file size in the ADD_FILE
// notification. This catches the fcopyfile race where Release fires before
// all data is flushed, sending a partial size to the peer.
func TestFUSE_LargeFileNotificationSize(t *testing.T) {
	tp := SetupFUSEPeerPair(t, 60*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	require := require.New(t)

	// 5 MB random data (large enough to expose partial-size bugs).
	size := 5 * 1024 * 1024
	data := make([]byte, size)
	rand.Read(data)
	originalMD5 := md5.Sum(data)

	// Alice writes via FUSE mount.
	alicePath := filepath.Join(tp.AliceMountDir, "large_notify_test.bin")
	require.NoError(os.WriteFile(alicePath, data, 0644))

	// Bob must see the file with correct size.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "/large_notify_test.bin", 15*time.Second)

	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	rf := tp.Bob.SyncTracker.RemoteFiles["/large_notify_test.bin"]
	notifiedSize := rf.Size
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()

	require.Equal(uint64(size), notifiedSize,
		"Bob received wrong file size in notification: got %d, want %d", notifiedSize, size)

	// Bob pulls and verifies MD5.
	pullDest := filepath.Join(tp.BobSaveDir, "large_notify_test.bin")
	require.NoError(tp.Bob.PullFile("/large_notify_test.bin", pullDest))

	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(size, len(pulled), "pulled file size mismatch")
	require.Equal(originalMD5, md5.Sum(pulled), "MD5 mismatch")
}

// Note: the fcopyfile deferred-notification path (HadEdits=false) cannot be
// unit-tested without macOS Finder's fcopyfile syscall. It is tested manually
// by drag-and-dropping large files into the FUSE mount from Finder.
