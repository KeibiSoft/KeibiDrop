// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// On-demand landings write the sidecar, and a flush at Release writes what the
// timer had not yet.
func TestSidecar_OnDemandLandingsPersist(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.SetCtx(context.Background())
	content := makePattern(ReadAheadBlock + ChunkSize)
	prov := &countingProvider{content: content}
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })
	require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/clip.bin", "clip.bin",
		remoteStat(int64(len(content)), time.Now().Add(-time.Hour)), 0))

	var st winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/clip.bin", &st, ^uint64(0)))
	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
	buf := make([]byte, 4096)
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 0, fi.Fh))
	d.OpenMapLock.RLock()
	f := d.OpenFileHandlers[fi.Fh].File
	d.OpenMapLock.RUnlock()
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	require.Equal(t, 0, d.Release("/clip.bin", fi.Fh))

	side := BitmapPath(filepath.Join(d.LocalDownloadFolder, "clip.bin"))
	bm, err := LoadChunkBitmap(side, int64(len(content)))
	require.NoError(t, err, "the sidecar exists after the first landing")
	require.True(t, bm.HasRange(0, 4096), "the landed range is marked in the sidecar")
	require.False(t, bm.IsComplete())
}

// The next session, before the announce: an open of the cache copy adopts the
// sidecar. A present chunk is served from disk, a missing one is fetched from the
// peer, and zeros are never served. Once the announce lands nothing is fetched
// again for what the copy already had.
func TestSidecar_OpenBeforeAnnounceAdoptsTheCacheCopy(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.SetCtx(context.Background())
	content := makePattern(ReadAheadBlock + ChunkSize)
	prov := &countingProvider{content: content}
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })

	// The copy from an earlier session: full size, sparse, chunk 0 present.
	cache := filepath.Join(d.LocalDownloadFolder, "clip.bin")
	require.NoError(t, os.WriteFile(cache, nil, 0o644))
	require.NoError(t, os.Truncate(cache, int64(len(content))))
	cf, err := os.OpenFile(cache, os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = cf.WriteAt(content[:ChunkSize], 0)
	require.NoError(t, err)
	require.NoError(t, cf.Close())
	bm := NewChunkBitmap(int64(len(content)))
	bm.Set(0)
	require.NoError(t, bm.Save(BitmapPath(cache)))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
	defer d.Release("/clip.bin", fi.Fh)
	d.OpenMapLock.RLock()
	f := d.OpenFileHandlers[fi.Fh].File
	d.OpenMapLock.RUnlock()
	require.NotNil(t, f.Bitmap, "the open adopted the sidecar")
	require.True(t, f.NotLocalSynced, "a partial copy is not a local file")
	require.False(t, f.LocalNewer)

	buf := make([]byte, 4096)
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 0, fi.Fh))
	require.Equal(t, content[:4096], buf)
	require.EqualValues(t, 0, prov.reads.Load(), "a present chunk is served from the copy")

	off := int64(ReadAheadBlock)
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, off, fi.Fh))
	require.Equal(t, content[off:off+4096], buf, "a missing chunk comes from the peer, never zeros")
	require.EqualValues(t, 1, prov.reads.Load())
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	// The announce lands: same size, origin mtime older than the copy.
	require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/clip.bin", "clip.bin",
		remoteStat(int64(len(content)), time.Now().Add(-time.Hour)), 0))
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 8192, fi.Fh))
	require.Equal(t, content[8192:8192+4096], buf)
	require.EqualValues(t, 1, prov.reads.Load(), "what the copy had is not fetched again after the announce")
}

// A complete copy with its sidecar is served locally at the next session, before
// and after the announce, with no fetch at all.
func TestSidecar_CompleteCopyIsNotFetchedAgain(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.SetCtx(context.Background())
	content := makePattern(2 * ChunkSize)
	prov := &countingProvider{content: content}
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })
	cache := filepath.Join(d.LocalDownloadFolder, "clip.bin")
	require.NoError(t, os.WriteFile(cache, content, 0o644))
	bm := NewChunkBitmap(int64(len(content)))
	bm.Set(0)
	bm.Set(1)
	require.True(t, bm.IsComplete())
	require.NoError(t, bm.Save(BitmapPath(cache)))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
	defer d.Release("/clip.bin", fi.Fh)
	buf := make([]byte, 4096)
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, int64(ChunkSize), fi.Fh))
	require.Equal(t, content[ChunkSize:ChunkSize+4096], buf)
	require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/clip.bin", "clip.bin",
		remoteStat(int64(len(content)), time.Now().Add(-time.Hour)), 0))
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 0, fi.Fh))
	require.Equal(t, content[:4096], buf)
	require.EqualValues(t, 0, prov.reads.Load(), "a complete copy costs no fetch")
}

// A rewrite drops the sidecar (the file is a local edit from here on) and an
// unlink removes it with the file.
func TestSidecar_TruncateAndUnlinkDropIt(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.SetCtx(context.Background())
	content := makePattern(2 * ChunkSize)
	cache := filepath.Join(d.LocalDownloadFolder, "clip.bin")
	require.NoError(t, os.WriteFile(cache, content, 0o644))
	bm := NewChunkBitmap(int64(len(content)))
	bm.Set(0)
	require.NoError(t, bm.Save(BitmapPath(cache)))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDWR | os.O_TRUNC
	require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
	_, err := os.Stat(BitmapPath(cache))
	require.True(t, os.IsNotExist(err), "a truncating open drops the sidecar")
	require.Equal(t, 0, d.Release("/clip.bin", fi.Fh))

	require.NoError(t, bm.Save(BitmapPath(cache)))
	require.Equal(t, 0, d.Unlink("/clip.bin"))
	_, err = os.Stat(BitmapPath(cache))
	require.True(t, os.IsNotExist(err), "unlink removes the sidecar with the file")
}
