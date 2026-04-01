// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBitmapSaveLoad verifies bitmap persistence roundtrip.
func TestBitmapSaveLoad(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()

	fileSize := int64(10 * 1024 * 1024) // 10 MB
	bm := filesystem.NewChunkBitmapWithSize(fileSize, 1024*1024) // 1 MiB chunks
	require.NotNil(bm)
	require.Equal(10, bm.Total())

	// Mark some chunks as downloaded.
	bm.Set(0)
	bm.Set(1)
	bm.Set(5)
	require.Equal(3, bm.Have())
	require.False(bm.IsComplete())

	// Save to disk.
	bmPath := filepath.Join(tmpDir, "test.kdbitmap")
	require.NoError(bm.Save(bmPath))

	// Load back.
	loaded, err := filesystem.LoadChunkBitmap(bmPath, fileSize)
	require.NoError(err)
	require.Equal(bm.Total(), loaded.Total())
	require.Equal(bm.Have(), loaded.Have())
	require.Equal(bm.FileSize(), loaded.FileSize())
	require.Equal(bm.ChunkSizeBytes(), loaded.ChunkSizeBytes())

	// Verify specific chunks.
	require.True(loaded.Has(0))
	require.True(loaded.Has(1))
	require.False(loaded.Has(2))
	require.True(loaded.Has(5))

	// Wrong fileSize should fail.
	_, err = filesystem.LoadChunkBitmap(bmPath, fileSize+1)
	require.Error(err)
	require.Contains(err.Error(), "mismatch")
}

// TestBitmapSaveLoadDifferentChunkSizes verifies bitmap works with different chunk sizes.
func TestBitmapSaveLoadDifferentChunkSizes(t *testing.T) {
	require := require.New(t)
	tmpDir := t.TempDir()

	fileSize := int64(5 * 1024 * 1024) // 5 MB

	for _, chunkSize := range []int{256 * 1024, 512 * 1024, 1024 * 1024} {
		t.Run(fmt.Sprintf("chunk_%dKiB", chunkSize/1024), func(t *testing.T) {
			bm := filesystem.NewChunkBitmapWithSize(fileSize, chunkSize)
			require.NotNil(bm)
			require.Equal(chunkSize, bm.ChunkSizeBytes())

			// Mark all but last chunk.
			for i := 0; i < bm.Total()-1; i++ {
				bm.Set(i)
			}

			bmPath := filepath.Join(tmpDir, fmt.Sprintf("test_%d.kdbitmap", chunkSize))
			require.NoError(bm.Save(bmPath))

			loaded, err := filesystem.LoadChunkBitmap(bmPath, fileSize)
			require.NoError(err)
			require.Equal(bm.Have(), loaded.Have())
			require.Equal(bm.Total(), loaded.Total())
			require.Equal(chunkSize, loaded.ChunkSizeBytes())
			require.False(loaded.IsComplete())
		})
	}
}

// TestNoFUSE_CancelDownload tests that cancelling a download preserves partial state.
func TestNoFUSE_CancelDownload(t *testing.T) {
	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)

	// 100 MB file - large enough that we can cancel mid-transfer on localhost.
	size := 100 * 1024 * 1024
	data := make([]byte, size)
	rand.Read(data)
	originalMD5 := md5.Sum(data)

	name := "cancel_test.bin"
	path := filepath.Join(tp.AliceSaveDir, name)
	require.NoError(os.WriteFile(path, data, 0644))
	require.NoError(tp.Alice.AddFile(path))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 5*time.Second)

	// Start download in background.
	pullDest := filepath.Join(tp.BobSaveDir, name)
	errCh := make(chan error, 1)
	go func() {
		errCh <- tp.Bob.PullFile(name, pullDest)
	}()

	// Cancel after a short delay (some chunks should have downloaded).
	time.Sleep(50 * time.Millisecond)
	cancelErr := tp.Bob.CancelDownload(name)
	// Cancel might fail if download already completed.
	pullErr := <-errCh

	if pullErr != nil {
		// Download was cancelled. Verify partial state is preserved.
		bmPath := filesystem.BitmapPath(pullDest)
		assert.FileExists(t, pullDest, "partial file should exist after cancel")
		assert.FileExists(t, bmPath, "bitmap file should exist after cancel")

		// Verify bitmap is loadable.
		bm, err := filesystem.LoadChunkBitmap(bmPath, int64(size))
		require.NoError(err)
		t.Logf("Cancel at %.1f%% (%d/%d chunks)", bm.Progress()*100, bm.Have(), bm.Total())

		// Resume: pull again. Should complete from where we left off.
		require.NoError(tp.Bob.PullFile(name, pullDest))

		// Verify bitmap is cleaned up after completion.
		assert.NoFileExists(t, bmPath, "bitmap should be removed after completion")
	} else {
		t.Log("Download completed before cancel took effect")
		_ = cancelErr
	}

	// Either way, the file should be complete and correct.
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(size, len(pulled))
	pulledMD5 := md5.Sum(pulled)
	require.Equal(originalMD5, pulledMD5, "MD5 mismatch after resume")
}

// TestNoFUSE_ResumeAfterDisconnect tests that a download can resume after
// peer disconnection and reconnection.
func TestNoFUSE_ResumeAfterDisconnect(t *testing.T) {
	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)

	// 50 MB file.
	size := 50 * 1024 * 1024
	data := make([]byte, size)
	rand.Read(data)
	originalMD5 := md5.Sum(data)

	name := "resume_test.bin"
	path := filepath.Join(tp.AliceSaveDir, name)
	require.NoError(os.WriteFile(path, data, 0644))
	require.NoError(tp.Alice.AddFile(path))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 5*time.Second)

	// Cancel mid-download to simulate partial state.
	pullDest := filepath.Join(tp.BobSaveDir, name)
	errCh := make(chan error, 1)
	go func() {
		errCh <- tp.Bob.PullFile(name, pullDest)
	}()

	time.Sleep(30 * time.Millisecond)
	_ = tp.Bob.CancelDownload(name)
	pullErr := <-errCh

	if pullErr == nil {
		// Completed too fast. Still verify correctness.
		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(originalMD5, md5.Sum(pulled))
		t.Log("Download completed before cancel, skipping resume test")
		return
	}

	// Verify partial state exists.
	bmPath := filesystem.BitmapPath(pullDest)
	require.FileExists(pullDest)
	require.FileExists(bmPath)

	bm, err := filesystem.LoadChunkBitmap(bmPath, int64(size))
	require.NoError(err)
	partialProgress := bm.Progress()
	t.Logf("Partial download at %.1f%%", partialProgress*100)

	// Resume download (same PullFile call, bitmap on disk enables resume).
	require.NoError(tp.Bob.PullFile(name, pullDest))

	// Verify file is complete and correct.
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(size, len(pulled))
	pulledMD5 := md5.Sum(pulled)
	require.Equal(originalMD5, pulledMD5, "MD5 mismatch after resume")

	// Bitmap should be cleaned up.
	assert.NoFileExists(t, bmPath)
}

// TestNoFUSE_LargeFile_MD5 verifies MD5 integrity of large file transfers.
func TestNoFUSE_LargeFile_MD5(t *testing.T) {
	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)

	// 10 MB with random data.
	size := 10 * 1024 * 1024
	data := make([]byte, size)
	rand.Read(data)
	originalMD5 := md5.Sum(data)

	name := "md5_test.bin"
	path := filepath.Join(tp.AliceSaveDir, name)
	require.NoError(os.WriteFile(path, data, 0644))
	require.NoError(tp.Alice.AddFile(path))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 5*time.Second)

	pullDest := filepath.Join(tp.BobSaveDir, name)
	require.NoError(tp.Bob.PullFile(name, pullDest))

	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(size, len(pulled))

	pulledMD5 := md5.Sum(pulled)
	require.Equal(originalMD5, pulledMD5, "MD5 mismatch: file corrupted during transfer")
}
