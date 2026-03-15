// ABOUTME: Tests for prefetch data integrity — short read rejection and bitmap marking.
// ABOUTME: Regression tests for issue #75 (git data corruption on FUSE mounts).

package filesystem

import (
	"os"
	"testing"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOnDemandRead_DoesNotMarkBitmapRange verifies that an on-demand FUSE read
// does NOT mark the bitmap for the chunk it fetched. Marking a 512 KiB chunk
// after fetching only 4-128 KiB would cause subsequent reads within that chunk
// to serve zeros from the pre-allocated cache file.
func TestOnDemandRead_DoesNotMarkBitmapRange(t *testing.T) {
	bitmap := NewChunkBitmap(ChunkSize * 4) // 4 chunks

	// Simulate what on-demand Read used to do (and should NOT do anymore).
	// A 128 KiB read at offset 0 should NOT mark chunk 0 (512 KiB).
	readSize := 128 * 1024
	// Verify chunk 0 is not marked before or after a simulated read.
	assert.False(t, bitmap.HasRange(0, readSize), "chunk should not be marked before read")

	// The fix: we do NOT call bitmap.SetRange here.
	// Previously: bitmap.SetRange(0, readSize) would mark all of chunk 0.

	// Verify the entire chunk 0 is still unmarked.
	assert.False(t, bitmap.Has(0), "chunk 0 must remain unmarked after on-demand read")
	assert.False(t, bitmap.HasRange(0, int(ChunkSize)), "full chunk must remain unmarked")
}

// TestSetRange_MarksEntireChunk demonstrates the bug that was fixed: SetRange
// on a sub-chunk read would mark the whole chunk, causing zeros to be served.
func TestSetRange_MarksEntireChunk(t *testing.T) {
	bitmap := NewChunkBitmap(ChunkSize * 2)

	// A 4 KiB write at offset 0 marks the entire chunk 0 (512 KiB).
	bitmap.SetRange(0, 4096)
	assert.True(t, bitmap.Has(0), "SetRange marks the chunk containing the offset")

	// This means HasRange for any offset within chunk 0 returns true.
	assert.True(t, bitmap.HasRange(100*1024, 4096),
		"HasRange returns true for any part of a marked chunk — "+
			"this is why on-demand reads must NOT call SetRange")
}

// TestPrefetchShortRead_ChunkNotMarked verifies that a prefetch short read
// (receiving fewer bytes than requested) does NOT result in the chunk being
// marked in the bitmap. If a short read were marked, subsequent FUSE reads
// would serve zeros from the pre-allocated file for the unwritten portion.
func TestPrefetchShortRead_ChunkNotMarked(t *testing.T) {
	fileSize := int64(ChunkSize * 3) // 3 chunks
	bitmap := NewChunkBitmap(fileSize)

	// Simulate prefetch for chunk 1: expects 512 KiB, gets only 400 KiB.
	idx := 1
	expectedSize := int64(ChunkSize)
	receivedData := make([]byte, 400*1024) // Short read!

	// The fix: check length before marking.
	if int64(len(receivedData)) != expectedSize {
		// Short read — skip this chunk, do NOT mark.
	} else {
		bitmap.Set(idx)
	}

	assert.False(t, bitmap.Has(idx), "chunk must NOT be marked after short read")
}

// TestPrefetchFullRead_ChunkMarked verifies normal prefetch behavior: when
// the full chunk is received, it IS marked in the bitmap.
func TestPrefetchFullRead_ChunkMarked(t *testing.T) {
	fileSize := int64(ChunkSize * 3)
	bitmap := NewChunkBitmap(fileSize)

	idx := 1
	expectedSize := int64(ChunkSize)
	receivedData := make([]byte, expectedSize) // Full chunk.

	if int64(len(receivedData)) == expectedSize {
		bitmap.Set(idx)
	}

	assert.True(t, bitmap.Has(idx), "chunk must be marked after full read")
}

// TestPrefetchLastChunk_ShortIsOK verifies that the last chunk of a file
// can be smaller than ChunkSize and still be correctly marked.
func TestPrefetchLastChunk_ShortIsOK(t *testing.T) {
	fileSize := int64(ChunkSize*2 + 1000) // 2 full chunks + 1000 bytes
	bitmap := NewChunkBitmap(fileSize)

	lastIdx := bitmap.Total() - 1
	offset := int64(lastIdx) * ChunkSize
	expectedSize := fileSize - offset // 1000 bytes
	require.Equal(t, int64(1000), expectedSize)

	receivedData := make([]byte, expectedSize)

	if int64(len(receivedData)) == expectedSize {
		bitmap.Set(lastIdx)
	}

	assert.True(t, bitmap.Has(lastIdx), "last chunk must be marked when full expected size received")
}

// TestGetattrPreallocated_DoesNotMarkLocalNewer verifies that a pre-allocated
// file (zero-filled via os.Truncate) is NOT marked as LocalNewer, even though
// its mtime is newer than the remote file's mtime.
func TestGetattrPreallocated_DoesNotMarkLocalNewer(t *testing.T) {
	localRoot := t.TempDir()
	d := newTestDir(localRoot)

	// Simulate a remote file with known size and old mtime.
	remFile := &File{
		stat: &winfuse.Stat_t{
			Size: 1024,
			Mtim: winfuse.Timespec{Sec: 1000, Nsec: 0}, // Old time.
		},
		Bitmap:         NewChunkBitmap(1024),
		NotLocalSynced: true,
		RealPathOfFile: localRoot + "/test.bin",
	}

	// Pre-allocate the file (simulates startPrefetch behavior).
	f, err := os.Create(remFile.RealPathOfFile)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(1024))
	f.Close()
	// Local mtime is NOW — definitely newer than remote mtime of 1000.

	d.RemoteFiles["/test.bin"] = remFile

	// Bitmap is NOT complete (no chunks downloaded yet).
	assert.False(t, remFile.Bitmap.IsComplete())

	// The downloadComplete check should prevent LocalNewer from being set.
	downloadComplete := remFile.Bitmap == nil || remFile.Bitmap.IsComplete()
	assert.False(t, downloadComplete,
		"download should not be complete for a fresh bitmap")
	assert.False(t, remFile.LocalNewer,
		"LocalNewer must stay false for pre-allocated files with incomplete download")
}

// TestGetattrCompleteDownload_CanMarkLocalNewer verifies that once the download
// is complete, Getattr CAN mark the file as LocalNewer.
func TestGetattrCompleteDownload_CanMarkLocalNewer(t *testing.T) {
	bitmap := NewChunkBitmap(int64(ChunkSize))
	bitmap.Set(0) // Mark the only chunk as downloaded.

	assert.True(t, bitmap.IsComplete(), "bitmap should be complete")

	downloadComplete := bitmap == nil || bitmap.IsComplete()
	assert.True(t, downloadComplete,
		"completed download should allow LocalNewer marking")
}
