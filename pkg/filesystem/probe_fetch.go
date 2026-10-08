// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build !android

package filesystem

import (
	"log/slog"
	"os"

	"github.com/zeebo/xxh3"
)

// streamProvenLocked is the read-ahead gate; the caller holds f.raMu. A stream
// is proven once it has consumed half a block. A thumbnailer or a filmstrip
// probe reads a few hundred KiB per touch and never gets there. An earlier
// rule armed after 2 MiB of dense consumption; live on macFUSE (2026-09-09)
// the kernel's own look-ahead reads after one probe met it, and a filmstrip
// of twenty probes primed the window and pulled 95% of the clip anyway.
func streamProvenLocked(f *File) bool {
	return f.raStreamActive && f.raStreamBytes >= int64(ReadAheadBlock)/2
}

// fetchUnit is the on-demand fetch unit: ProbeFetch with read-ahead on, the
// whole block with it off (the block fetch is then the only batching left).
func (d *Dir) fetchUnit() int64 {
	if d.ReadAheadWindowBlocks <= 0 {
		return int64(ReadAheadBlock)
	}
	return int64(ProbeFetch)
}

// beginFetchSpan makes the caller the leader for the units of [start, end)
// that nobody owns yet, starting at start, and returns the end of what it
// owns. It stops before a unit another fetch owns or that already landed; the
// caller fetches [start, owned) and, when its request runs past that, waits
// for the other owner (see fetchAt). When the first unit is already owned the
// caller joins that fetch instead (leader false, owned 0).
func (f *File) beginFetchSpan(start, end, unit int64, bm *ChunkBitmap) (bf *blockFetch, leader bool, owned int64) {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	if f.inflight == nil {
		f.inflight = make(map[int64]*blockFetch)
	}
	if first := f.inflight[start]; first != nil {
		return first, false, 0
	}
	bf = &blockFetch{done: make(chan struct{})}
	owned = start
	for u := start; u < end; u += unit {
		if f.inflight[u] != nil {
			break
		}
		if u > start && bm != nil && bm.HasRange(u, int(min(unit, end-u))) {
			break
		}
		f.inflight[u] = bf
		bf.keys = append(bf.keys, u)
		owned = min(u+unit, end)
	}
	return bf, true, owned
}

// fetchAt returns the fetch that owns the unit at start, or nil.
func (f *File) fetchAt(start int64) *blockFetch {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	return f.inflight[start]
}

// markCached lands a fetched range in the cache file chunk by chunk and marks
// every chunk fully inside it, with its fingerprint. A chunk already present
// is skipped: an on-demand read landed it, or a local write holds it
// (fillForWrite), and the peer's bytes must never go over the app's. Returns
// the bytes written. base is chunk-aligned (fetch bounds are multiples of
// ChunkSize); a partial tail chunk is written, never marked.
func markCached(cacheFD *os.File, bm *ChunkBitmap, data []byte, base, remoteFileSize int64) (int, error) {
	if bm == nil {
		n, err := cacheFD.WriteAt(data, base)
		return n, err
	}
	cs := int64(ChunkSize)
	end := base + int64(len(data))
	written := 0
	for pos := base; pos < end; {
		c := int(pos / cs)
		chunkBegin := int64(c) * cs
		chunkEnd := min(chunkBegin+cs, end)
		if remoteFileSize > 0 && chunkEnd > remoteFileSize {
			chunkEnd = remoteFileSize
		}
		if chunkEnd <= pos {
			break
		}
		if bm.Has(c) {
			pos = chunkEnd
			continue
		}
		n, err := cacheFD.WriteAt(data[pos-base:chunkEnd-base], pos)
		written += n
		if err != nil {
			return written, err
		}
		full := chunkEnd == chunkBegin+cs || (remoteFileSize > 0 && chunkEnd == remoteFileSize)
		if pos == chunkBegin && full {
			bm.Set(c)
			bm.SetHash(c, xxh3.Hash(data[pos-base:chunkEnd-base]))
		}
		pos = chunkEnd
	}
	return written, nil
}

// preadCached serves a request from the cache file once its chunks are marked,
// clamped to the remote size (the pre-allocated file may hold bytes past it).
func (d *Dir) preadCached(logger *slog.Logger, fd int, buff []byte, offset, remoteFileSize int64) int {
	n, err := platPread(fd, buff, offset)
	if err != nil {
		logger.Error("Local pread failed after a fetch landed", "error", err)
		return int(convertOsErrToSyscallErrno("pread", err))
	}
	if remoteFileSize > 0 && offset+int64(n) > remoteFileSize {
		n = int(remoteFileSize - offset)
		if n < 0 {
			n = 0
		}
	}
	return n
}

// beginBlockFetch registers one key: the sibling warmer's whole small file
// lives in unit 0. Kept as the single-key form of beginFetchSpan.
func (f *File) beginBlockFetch(start int64) (bf *blockFetch, leader bool) {
	f.fetchMu.Lock()
	defer f.fetchMu.Unlock()
	if f.inflight == nil {
		f.inflight = make(map[int64]*blockFetch)
	}
	if bf = f.inflight[start]; bf != nil {
		return bf, false
	}
	bf = &blockFetch{done: make(chan struct{}), keys: []int64{start}}
	f.inflight[start] = bf
	return bf, true
}

// finishBlockFetch is finishFetch for a fetch registered under one key.
func (f *File) finishBlockFetch(start int64, bf *blockFetch, err error) {
	if len(bf.keys) == 0 {
		bf.keys = []int64{start}
	}
	f.finishFetch(bf, err)
}
