// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSESync runs FUSE filesystem sync correctness tests.
// Alice=FUSE, Bob=no-FUSE (single mount per process limitation).
// These tests probe edge cases in the sync layer.
func TestFUSESync(t *testing.T) {
	skipIfNoFUSE(t)

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	t.Run("DeleteLocalFile", func(t *testing.T) {
		// Alice creates file on FUSE mount → Bob sees it → Alice deletes → Bob should no longer see it.
		require := require.New(t)

		content := []byte("file to be deleted")
		alicePath := filepath.Join(tp.AliceMountDir, "delete_me.txt")
		require.NoError(os.WriteFile(alicePath, content, 0644))

		// Bob should see the file in his SyncTracker.
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/delete_me.txt", 15*time.Second)

		// Verify Bob can pull it.
		pullDest := filepath.Join(tp.BobSaveDir, "delete_me.txt")
		require.NoError(tp.Bob.PullFile("/delete_me.txt", pullDest))
		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(content, pulled)

		// Now Alice deletes the file from her FUSE mount.
		require.NoError(os.Remove(alicePath))

		// Bob should no longer see the file in RemoteFiles.
		WaitForCondition(t, 10*time.Second, 200*time.Millisecond, func() bool {
			tp.Bob.SyncTracker.RemoteFilesMu.RLock()
			defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
			_, exists := tp.Bob.SyncTracker.RemoteFiles["/delete_me.txt"]
			return !exists
		}, "waiting for Bob to see file removal after Alice deleted it")
	})

	t.Run("RenameFile", func(t *testing.T) {
		// Alice creates file on FUSE mount → Bob sees it → Alice renames → Bob sees new name.
		require := require.New(t)

		content := []byte("file to be renamed")
		alicePath := filepath.Join(tp.AliceMountDir, "old_name.txt")
		require.NoError(os.WriteFile(alicePath, content, 0644))

		// Bob should see the file.
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/old_name.txt", 15*time.Second)

		// Alice renames the file.
		newPath := filepath.Join(tp.AliceMountDir, "new_name.txt")
		require.NoError(os.Rename(alicePath, newPath))

		// Bob should see the file under the new name.
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/new_name.txt", 10*time.Second)

		// Old name should be gone.
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		_, oldExists := tp.Bob.SyncTracker.RemoteFiles["/old_name.txt"]
		tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		require.False(oldExists, "old file name should be removed after rename")

		// Verify content is still accessible under new name.
		pullDest := filepath.Join(tp.BobSaveDir, "new_name.txt")
		require.NoError(tp.Bob.PullFile("/new_name.txt", pullDest))
		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(content, pulled)
	})

	t.Run("EmptyFile", func(t *testing.T) {
		// Alice creates an empty file on FUSE mount → Bob should see it.
		// Current behavior: Release() skips notification for size=0 files.
		require := require.New(t)

		alicePath := filepath.Join(tp.AliceMountDir, "empty.txt")
		require.NoError(os.WriteFile(alicePath, []byte{}, 0644))

		// Bob should see the empty file in SyncTracker.
		// This may fail if the size=0 guard in Release() prevents notification.
		WaitForCondition(t, 10*time.Second, 200*time.Millisecond, func() bool {
			tp.Bob.SyncTracker.RemoteFilesMu.RLock()
			defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
			_, exists := tp.Bob.SyncTracker.RemoteFiles["/empty.txt"]
			return exists
		}, "waiting for Bob to see empty file created by Alice on FUSE mount")
	})

	t.Run("LargeBinaryFromRemote", func(t *testing.T) {
		// Simulates video playback: Bob shares a large binary file (like .mov),
		// Alice reads it from her FUSE mount. Verifies byte-for-byte integrity
		// through the gRPC streaming + FUSE read path.
		require := require.New(t)

		// Generate 1.8 MB of random binary data (simulates .mov file).
		data := make([]byte, 1800*1024)
		_, err := rand.Read(data)
		require.NoError(err)

		// Bob adds the file via no-FUSE API.
		bobPath := filepath.Join(tp.BobSaveDir, "video.mov")
		require.NoError(os.WriteFile(bobPath, data, 0644))
		require.NoError(tp.Bob.AddFile(bobPath))

		// Alice should see it on her FUSE mount.
		alicePath := filepath.Join(tp.AliceMountDir, "video.mov")
		WaitForFileOnMount(t, alicePath, 15*time.Second)

		// Read the full file from FUSE mount (triggers gRPC Read stream to Bob).
		read, err := os.ReadFile(alicePath)
		require.NoError(err)
		require.Equal(len(data), len(read), "file size mismatch")
		require.Equal(data, read, "file content mismatch — streaming corruption")
	})

	t.Run("OverwriteFile", func(t *testing.T) {
		// Alice writes a file → Bob sees it → Alice overwrites with different content.
		// Verify Bob sees the update (EditFile notification).
		require := require.New(t)

		initial := []byte("version 1 content")
		alicePath := filepath.Join(tp.AliceMountDir, "overwrite.txt")
		require.NoError(os.WriteFile(alicePath, initial, 0644))

		// Bob sees initial version.
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/overwrite.txt", 15*time.Second)

		pullDest := filepath.Join(tp.BobSaveDir, "overwrite_v1.txt")
		require.NoError(tp.Bob.PullFile("/overwrite.txt", pullDest))
		v1, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(initial, v1)

		// Alice overwrites with new content.
		updated := []byte("version 2 content - longer")
		require.NoError(os.WriteFile(alicePath, updated, 0644))

		// Give time for notification to propagate.
		time.Sleep(2 * time.Second)

		// Bob pulls again — should get updated content.
		pullDest2 := filepath.Join(tp.BobSaveDir, "overwrite_v2.txt")
		require.NoError(tp.Bob.PullFile("/overwrite.txt", pullDest2))
		v2, err := os.ReadFile(pullDest2)
		require.NoError(err)
		require.Equal(updated, v2)
	})
}
