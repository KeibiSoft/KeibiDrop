// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: A remote file whose cache copy survived a daemon restart must cache
// ABOUTME: fetched blocks once the peer announces it; every read re-fetched before.

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

// After a daemon restart the save folder still holds the cache copy of a
// remote file: full size, sparse, with the cache writer's mtime. A stat on the
// mount before the peer's announce registers that path from disk, without a
// block bitmap, and the announce (same size, older origin mtime) keeps the
// entry. Without a bitmap every read fetched its 16 MiB block again and could
// never mark it: decoding a 21 MB clip cost 386 MiB (measured 2026-09-06).
func TestAnnounce_DiskEntryWithoutBitmapCachesBlocks(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.SetCtx(context.Background())
	content := makePattern(ReadAheadBlock + ChunkSize) // one full block and a tail
	prov := &countingProvider{content: content}
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })

	cache := filepath.Join(d.LocalDownloadFolder, "clip.bin")
	require.NoError(t, os.WriteFile(cache, nil, 0o644))
	require.NoError(t, os.Truncate(cache, int64(len(content))))

	// Finder stats the path before the announce lands.
	var st winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/clip.bin", &st, ^uint64(0)))

	// The peer announces the same size with the origin mtime, older than the copy.
	origin := time.Now().Add(-time.Hour)
	require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/clip.bin", "clip.bin",
		remoteStat(int64(len(content)), origin), 0))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
	defer d.Release("/clip.bin", fi.Fh)
	d.OpenMapLock.RLock()
	f := d.OpenFileHandlers[fi.Fh].File
	d.OpenMapLock.RUnlock()

	buf := make([]byte, 4096)
	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 0, fi.Fh))
	require.Equal(t, content[:4096], buf)
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	require.Equal(t, len(buf), d.Read("/clip.bin", buf, 1<<20, fi.Fh))
	require.Equal(t, content[1<<20:1<<20+4096], buf)
	require.EqualValues(t, 1, prov.reads.Load(),
		"the second read lands inside the fetched block and must be served locally")
	require.NotNil(t, f.Bitmap, "the announce must attach a bitmap to an entry registered from disk")
}

// The other order of the same restart: the announce lands first (fresh entry in
// the remote map), then the first open finds a cache copy on disk and no entry
// in the open map. It created a second, bitmap-less file object for the path
// and every read on it fetched again: a take pulled 7 GB for 557 MB of clips
// (2026-09-06 16:47). The open must adopt the announced entry instead.
func TestOpen_FreshAnnounceThenOpenWithCacheCopyCachesBlocks(t *testing.T) {
	for _, statFirst := range []bool{false, true} {
		d := newTestDir(t.TempDir())
		d.SetCtx(context.Background())
		content := makePattern(ReadAheadBlock + ChunkSize)
		prov := &countingProvider{content: content}
		d.SetStreamProvider(func() types.FileStreamProvider { return prov })

		cache := filepath.Join(d.LocalDownloadFolder, "clip.bin")
		require.NoError(t, os.WriteFile(cache, nil, 0o644))
		require.NoError(t, os.Truncate(cache, int64(len(content))))

		origin := time.Now().Add(-time.Hour)
		require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/clip.bin", "clip.bin",
			remoteStat(int64(len(content)), origin), 0))
		if statFirst {
			var st winfuse.Stat_t
			require.Equal(t, 0, d.Getattr("/clip.bin", &st, ^uint64(0)))
		}

		fi := &winfuse.FileInfo_t{}
		fi.Flags = os.O_RDONLY
		require.Equal(t, 0, d.OpenEx("/clip.bin", fi))
		d.OpenMapLock.RLock()
		f := d.OpenFileHandlers[fi.Fh].File
		d.OpenMapLock.RUnlock()

		buf := make([]byte, 4096)
		require.Equal(t, len(buf), d.Read("/clip.bin", buf, 0, fi.Fh))
		require.Equal(t, content[:4096], buf)
		waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
		require.Equal(t, len(buf), d.Read("/clip.bin", buf, 1<<20, fi.Fh))
		require.Equal(t, content[1<<20:1<<20+4096], buf)
		require.EqualValues(t, 1, prov.reads.Load(), "statFirst=%v: the second read inside the fetched block must be local", statFirst)
		require.Equal(t, 0, d.Release("/clip.bin", fi.Fh))
	}
}
