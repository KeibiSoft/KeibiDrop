// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"crypto/md5" //#nosec G501
	"crypto/rand"
	"fmt"
	"io"
	mathrand "math/rand"
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

	t.Run("RandomJumpDownload_QuickTimePattern", func(t *testing.T) {
		// Simulates QuickTime's read pattern on a .mov file:
		// 1. Read header (first 4KB)
		// 2. Read trailer (last ~13KB)
		// 3. Skip middle ~80%
		// Before the hybrid download fix, SaveAlice would have correct size
		// but only header+trailer bytes — middle would be zeros = corruption.
		// With the background prefetch, the full file should be available.
		require := require.New(t)

		// Generate 2 MiB of random binary data (simulates .mov file).
		fileSize := 2 * 1024 * 1024
		data := make([]byte, fileSize)
		_, err := rand.Read(data)
		require.NoError(err)

		// Bob adds the file via no-FUSE API.
		bobPath := filepath.Join(tp.BobSaveDir, "quicktime.mov")
		require.NoError(os.WriteFile(bobPath, data, 0644))
		require.NoError(tp.Bob.AddFile(bobPath))

		// Alice should see it on her FUSE mount.
		alicePath := filepath.Join(tp.AliceMountDir, "quicktime.mov")
		WaitForFileOnMount(t, alicePath, 15*time.Second)

		// Simulate QuickTime: read only header + trailer (random jumps).
		f, err := os.Open(alicePath)
		require.NoError(err)

		// Read header: first 4KB
		header := make([]byte, 4096)
		n, err := f.ReadAt(header, 0)
		require.NoError(err)
		require.Equal(4096, n)
		require.Equal(data[:4096], header, "header bytes mismatch")

		// Read trailer: last 13KB
		trailerOffset := int64(fileSize - 13*1024)
		trailer := make([]byte, 13*1024)
		n, err = f.ReadAt(trailer, trailerOffset)
		require.NoError(err)
		require.Equal(13*1024, n)
		require.Equal(data[trailerOffset:], trailer, "trailer bytes mismatch")

		// Random jumps in the middle (like seeking in a video player)
		rng := mathrand.New(mathrand.NewSource(42))
		for i := 0; i < 5; i++ {
			jumpOffset := int64(rng.Intn(fileSize - 8192))
			chunk := make([]byte, 8192)
			n, err := f.ReadAt(chunk, jumpOffset)
			require.NoError(err)
			require.Equal(8192, n)
			require.Equal(data[jumpOffset:jumpOffset+8192], chunk,
				"random jump read mismatch at offset %d", jumpOffset)
		}

		f.Close()

		// Wait for background prefetch to complete the full download.
		// The prefetch goroutine fills all chunks sequentially.
		aliceSavePath := filepath.Join(tp.AliceSaveDir, "quicktime.mov")
		WaitForCondition(t, 30*time.Second, 500*time.Millisecond, func() bool {
			info, err := os.Stat(aliceSavePath)
			if err != nil {
				return false
			}
			if info.Size() != int64(fileSize) {
				return false
			}
			// Verify MD5 matches — this catches the zeros-in-the-middle bug.
			saved, err := os.ReadFile(aliceSavePath)
			if err != nil {
				return false
			}
			return md5.Sum(saved) == md5.Sum(data) //#nosec G401
		}, "waiting for complete download with correct MD5")

		// Final verification: full file read from FUSE mount should match.
		fullRead, err := os.ReadFile(alicePath)
		require.NoError(err)
		require.Equal(len(data), len(fullRead), "full read size mismatch")

		origMD5 := fmt.Sprintf("%x", md5.Sum(data))   //#nosec G401
		readMD5 := fmt.Sprintf("%x", md5.Sum(fullRead)) //#nosec G401
		require.Equal(origMD5, readMD5, "MD5 mismatch — file corruption detected")
	})

	t.Run("LargeBinaryWithRandomReads", func(t *testing.T) {
		// Stress test: larger file with many random reads at various offsets,
		// simulating a media player seeking through a video file.
		require := require.New(t)

		fileSize := 3 * 1024 * 1024 // 3 MiB
		data := make([]byte, fileSize)
		_, err := rand.Read(data)
		require.NoError(err)

		bobPath := filepath.Join(tp.BobSaveDir, "random_seek.bin")
		require.NoError(os.WriteFile(bobPath, data, 0644))
		require.NoError(tp.Bob.AddFile(bobPath))

		alicePath := filepath.Join(tp.AliceMountDir, "random_seek.bin")
		WaitForFileOnMount(t, alicePath, 15*time.Second)

		f, err := os.Open(alicePath)
		require.NoError(err)

		// 20 random reads at various offsets and sizes.
		rng := mathrand.New(mathrand.NewSource(99))
		for i := 0; i < 20; i++ {
			readSize := 1024 + rng.Intn(64*1024) // 1KB to 64KB
			maxOffset := fileSize - readSize
			if maxOffset <= 0 {
				maxOffset = 1
			}
			offset := int64(rng.Intn(maxOffset))

			buf := make([]byte, readSize)
			n, err := f.ReadAt(buf, offset)
			if err != nil && err != io.EOF {
				require.NoError(err)
			}
			require.Equal(data[offset:offset+int64(n)], buf[:n],
				"random read mismatch at offset=%d size=%d", offset, readSize)
		}

		f.Close()

		// Wait for background prefetch, then verify full integrity.
		aliceSavePath := filepath.Join(tp.AliceSaveDir, "random_seek.bin")
		WaitForCondition(t, 30*time.Second, 500*time.Millisecond, func() bool {
			saved, err := os.ReadFile(aliceSavePath)
			if err != nil || len(saved) != fileSize {
				return false
			}
			return md5.Sum(saved) == md5.Sum(data) //#nosec G401
		}, "waiting for full download of random_seek.bin")

		origMD5 := fmt.Sprintf("%x", md5.Sum(data))
		saved, err := os.ReadFile(aliceSavePath)
		require.NoError(err)
		savedMD5 := fmt.Sprintf("%x", md5.Sum(saved))
		require.Equal(origMD5, savedMD5, "saved file MD5 mismatch")
	})
}
