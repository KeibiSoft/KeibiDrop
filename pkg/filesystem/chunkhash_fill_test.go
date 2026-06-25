// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: TDD tests for per-chunk hash filling at the two FUSE cache-write points.
// ABOUTME: Verifies SetHash is called at both the async writer goroutine and prefetchRange.

package filesystem

import (
	"testing"
	"time"

	"github.com/zeebo/xxh3"
)

// TestChunkHashFill_AsyncWriter checks that the async cache writer goroutine
// inside Dir.Read fills hashes for every chunk it writes, including interior
// and final-partial chunks (span parity with the T2 server handler).
func TestChunkHashFill_AsyncWriter(t *testing.T) {
	// File size: 2 blocks + 100 KiB so the final chunk is partial.
	const extra = 100 * 1024
	fileSize := 2*ReadAheadBlock + extra
	content := makePattern(fileSize)
	root, fh, _, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 0 // prefetch off; pure on-demand
	f := root.OpenFileHandlers[fh].File

	cs := int64(ChunkSize)

	// Read all of block 0 to trigger the on-demand fetch and async writer.
	buf := make([]byte, 128*1024)
	for off := int64(0); off < int64(ReadAheadBlock); off += int64(len(buf)) {
		if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
			t.Fatalf("read at %d returned %d", off, n)
		}
	}
	// Let the async writer goroutine finish.
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	// Check an interior chunk (chunk 1, fully inside block 0).
	c := 1
	chunkBegin := int64(c) * cs
	chunkEnd := chunkBegin + cs // interior; not clamped
	want := xxh3.Hash(content[chunkBegin:chunkEnd])
	got, ok := f.Bitmap.Hash(c)
	if !ok {
		t.Fatalf("async writer: Hash(%d) not set after on-demand read", c)
	}
	if got != want {
		t.Fatalf("async writer: chunk %d hash mismatch: got %x, want %x", c, got, want)
	}

	// Now read block 1 (the final partial block) and verify the last chunk.
	for off := int64(ReadAheadBlock); off < int64(fileSize); off += int64(len(buf)) {
		n := len(buf)
		if int64(off)+int64(n) > int64(fileSize) {
			n = fileSize - int(off)
		}
		if nr := root.Read("/f.bin", buf[:n], off, fh); nr <= 0 {
			t.Fatalf("read at %d returned %d", off, nr)
		}
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	// Last chunk: spans [lastChunkBegin, fileSize) -- the partial span.
	total := f.Bitmap.Total()
	lastChunk := total - 1
	lastChunkBegin := int64(lastChunk) * cs
	wantLast := xxh3.Hash(content[lastChunkBegin:fileSize])
	gotLast, okLast := f.Bitmap.Hash(lastChunk)
	if !okLast {
		t.Fatalf("async writer: last chunk Hash(%d) not set", lastChunk)
	}
	if gotLast != wantLast {
		t.Fatalf("async writer: last chunk hash mismatch: got %x, want %x (partial span parity)", gotLast, wantLast)
	}
}

// TestChunkHashFill_PrefetchRange checks that prefetchRange fills hashes for
// chunks it writes, including a chunk that was prefetched but never directly read.
func TestChunkHashFill_PrefetchRange(t *testing.T) {
	// 3 blocks so block 1 can be prefetched without a direct read.
	fileSize := 3 * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, _, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4 // enable read-ahead so prefetchRange runs
	f := root.OpenFileHandlers[fh].File

	cs := int64(ChunkSize)
	ra := int64(ReadAheadBlock)

	// Read only block 0 to trigger a prefetch of block 1.
	buf := make([]byte, 128*1024)
	for off := int64(0); off < ra; off += int64(len(buf)) {
		if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
			t.Fatalf("read at %d returned %d", off, n)
		}
	}

	// Wait for prefetch goroutine and async writers to finish.
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })

	// The first chunk inside block 1 must have been hashed by prefetchRange.
	c := int(ra / cs)
	chunkBegin := int64(c) * cs
	chunkEnd := chunkBegin + cs
	if int64(fileSize) > 0 && chunkEnd > int64(fileSize) {
		chunkEnd = int64(fileSize)
	}
	want := xxh3.Hash(content[chunkBegin:chunkEnd])
	got, ok := f.Bitmap.Hash(c)
	if !ok {
		t.Fatalf("prefetchRange: Hash(%d) not set for prefetched-but-not-read chunk", c)
	}
	if got != want {
		t.Fatalf("prefetchRange: chunk %d hash mismatch: got %x, want %x", c, got, want)
	}
}

// TestChunkHashFill_PrefetchPartialChunk checks that prefetchRange hashes the
// partial final chunk of a partial final block fetched via read-ahead, without
// any direct read of that chunk.
func TestChunkHashFill_PrefetchPartialChunk(t *testing.T) {
	// 2 full blocks + 100 KiB: block 2 is partial and holds a single partial chunk.
	const extra = 100 * 1024
	fileSize := 2*ReadAheadBlock + extra
	content := makePattern(fileSize)
	root, fh, _, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	// Window of 4 ensures reading block 0 prefetches blocks 1 and 2.
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	cs := int64(ChunkSize)
	ra := int64(ReadAheadBlock)

	// Read only block 0; this triggers prefetchRange which fetches block 1 and
	// block 2 (the partial final block) without any direct read of those blocks.
	buf := make([]byte, 128*1024)
	for off := int64(0); off < ra; off += int64(len(buf)) {
		if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
			t.Fatalf("read at %d returned %d", off, n)
		}
	}

	// Wait for prefetch goroutine and async writers to finish.
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })

	// The only chunk in block 2 is partial: [2*ra, fileSize).
	lastChunk := int(int64(2) * ra / cs) // first (and only) chunk of block 2
	lastChunkBegin := int64(lastChunk) * cs
	wantLast := xxh3.Hash(content[lastChunkBegin:fileSize])
	gotLast, okLast := f.Bitmap.Hash(lastChunk)
	if !okLast {
		t.Fatalf("prefetchRange: partial last chunk Hash(%d) not set", lastChunk)
	}
	if gotLast != wantLast {
		t.Fatalf("prefetchRange: partial last chunk %d hash mismatch: got %x, want %x (partial span parity)", lastChunk, gotLast, wantLast)
	}
}
