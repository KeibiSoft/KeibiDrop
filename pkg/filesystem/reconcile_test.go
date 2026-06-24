// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: unit tests for ChunkBitmap.HasHashes and reconcileBitmap.
// ABOUTME: covers all 9 decision-table cases from the task brief.

package filesystem

import "testing"

const cs = 512 * 1024 // 512 KiB

// buildOld creates a bitmap with the given fileSize, sets+hashes the specified
// chunks, and leaves the rest absent.
func buildOld(fileSize int64, chunkHashes map[int]uint64) *ChunkBitmap {
	b := NewChunkBitmapWithSize(fileSize, cs)
	for c, h := range chunkHashes {
		b.Set(c)
		b.SetHash(c, h)
	}
	return b
}

// TestHasHashes_Fresh checks that a fresh bitmap without any SetHash call returns false.
func TestHasHashes_Fresh(t *testing.T) {
	b := NewChunkBitmapWithSize(cs*2, cs)
	if b.HasHashes() {
		t.Fatal("fresh bitmap must return false from HasHashes")
	}
}

// TestHasHashes_AfterSetHash checks that HasHashes returns true after SetHash.
func TestHasHashes_AfterSetHash(t *testing.T) {
	b := NewChunkBitmapWithSize(cs*2, cs)
	b.Set(0)
	b.SetHash(0, 0xdeadbeef)
	if !b.HasHashes() {
		t.Fatal("expected true after SetHash")
	}
}

// Case 1: in-place same-size edit. All chunks match except chunk k.
func TestReconcileBitmap_SameSizeEdit(t *testing.T) {
	// 4 chunks, all set+hashed. Peer agrees on all but chunk 2.
	size := int64(cs * 4)
	old := buildOld(size, map[int]uint64{0: 0xA, 1: 0xB, 2: 0xC, 3: 0xD})

	peerHashes := map[int]uint64{0: 0xA, 1: 0xB, 2: 0xFF, 3: 0xD} // chunk 2 changed

	nb, ok := reconcileBitmap(old, size, size, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantHashes := map[int]uint64{0: 0xA, 1: 0xB, 3: 0xD}
	for _, c := range []int{0, 1, 3} {
		if !nb.Has(c) {
			t.Fatalf("chunk %d should be kept (hash matched)", c)
		}
		h, got := nb.Hash(c)
		if !got {
			t.Fatalf("chunk %d should carry hash in nb", c)
		}
		if h != wantHashes[c] {
			t.Fatalf("chunk %d hash = %#x, want %#x", c, h, wantHashes[c])
		}
	}
	if nb.Has(2) {
		t.Fatal("chunk 2 should be absent (hash differed)")
	}
}

// Case 2: append/grow. All old hashes match; grown-region chunks absent.
// 3 old chunks, grows to 5. oldSize=3*cs (chunk-aligned), so commonEnd=3*cs.
// Chunks 0,1,2 satisfy (c+1)*cs <= 3*cs and are fully inside the common prefix; all kept.
// Chunks 3,4 are in the grown region and must be absent.
func TestReconcileBitmap_Grow(t *testing.T) {
	oldSize := int64(cs * 3)
	newSize := int64(cs * 5)
	old := buildOld(oldSize, map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3})

	peerHashes := map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3, 3: 0x4, 4: 0x5}

	nb, ok := reconcileBitmap(old, oldSize, newSize, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantHashes := map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3}
	// Chunks 0,1,2 fully inside common prefix (commonEnd=3*cs); all match => kept.
	for _, c := range []int{0, 1, 2} {
		if !nb.Has(c) {
			t.Fatalf("chunk %d should be kept", c)
		}
		h, got := nb.Hash(c)
		if !got {
			t.Fatalf("chunk %d should carry hash in nb", c)
		}
		if h != wantHashes[c] {
			t.Fatalf("chunk %d hash = %#x, want %#x", c, h, wantHashes[c])
		}
	}
	// Chunks 3,4 are grown region, must be absent.
	for _, c := range []int{3, 4} {
		if nb.Has(c) {
			t.Fatalf("chunk %d should be absent (grown region)", c)
		}
	}
}

// Case 3: truncate/shrink. newSize < oldSize. Fully-covered matching chunks within
// [0,newSize) are kept; nothing at or beyond.
// oldSize=5*cs, newSize=3*cs. commonEnd=3*cs. Chunk 2: (3)*cs<=3*cs, kept.
// Chunks 3,4 beyond truncation.
func TestReconcileBitmap_Shrink(t *testing.T) {
	oldSize := int64(cs * 5)
	newSize := int64(cs * 3)
	old := buildOld(oldSize, map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3, 3: 0x4, 4: 0x5})

	peerHashes := map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3}

	nb, ok := reconcileBitmap(old, oldSize, newSize, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	wantHashes := map[int]uint64{0: 0x1, 1: 0x2, 2: 0x3}
	// Chunks 0,1,2 within common prefix.
	for _, c := range []int{0, 1, 2} {
		if !nb.Has(c) {
			t.Fatalf("chunk %d should be kept", c)
		}
		h, got := nb.Hash(c)
		if !got {
			t.Fatalf("chunk %d should carry hash in nb", c)
		}
		if h != wantHashes[c] {
			t.Fatalf("chunk %d hash = %#x, want %#x", c, h, wantHashes[c])
		}
	}
	// Chunks 3,4 truncated.
	for _, c := range []int{3, 4} {
		if nb.Has(c) {
			t.Fatalf("chunk %d should be absent (truncated)", c)
		}
	}
}

// Case 4: boundary chunk straddling min(old,new) is never kept even if hash "matches".
// Make oldSize non-chunk-aligned so the boundary chunk is partial.
// oldSize=2*cs+100, newSize=3*cs. commonEnd=2*cs+100.
// Chunk 0: (1)*cs=cs <= 2*cs+100, kept if matching.
// Chunk 1: (2)*cs=2*cs <= 2*cs+100, kept if matching.
// Chunk 2: (3)*cs=3*cs > 2*cs+100, boundary => never kept.
func TestReconcileBitmap_BoundaryChunkNotKept(t *testing.T) {
	oldSize := int64(cs*2 + 100)
	newSize := int64(cs * 3)
	old := buildOld(oldSize, map[int]uint64{0: 0xA, 1: 0xB, 2: 0xC})

	// Peer reports same hash for chunk 2; it must still be dropped.
	peerHashes := map[int]uint64{0: 0xA, 1: 0xB, 2: 0xC}

	nb, ok := reconcileBitmap(old, oldSize, newSize, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !nb.Has(0) || !nb.Has(1) {
		t.Fatal("chunks 0 and 1 should be kept")
	}
	if nb.Has(2) {
		t.Fatal("boundary chunk 2 must not be kept even if hash matches")
	}
}

// Case 5: old.HasHashes() == false => (nil, false).
func TestReconcileBitmap_NoHashes(t *testing.T) {
	size := int64(cs * 2)
	old := NewChunkBitmapWithSize(size, cs)
	old.Set(0) // set bit but no hash

	nb, ok := reconcileBitmap(old, size, size, map[int]uint64{0: 0xA})
	if ok || nb != nil {
		t.Fatal("expected (nil, false) when old has no hashes")
	}
}

// Case 6: len(peerHashes) == 0 => (nil, false).
func TestReconcileBitmap_EmptyPeerHashes(t *testing.T) {
	size := int64(cs * 2)
	old := buildOld(size, map[int]uint64{0: 0xA})

	nb, ok := reconcileBitmap(old, size, size, map[int]uint64{})
	if ok || nb != nil {
		t.Fatal("expected (nil, false) when peerHashes is empty")
	}
}

// Case 7: old == nil => (nil, false).
func TestReconcileBitmap_NilOld(t *testing.T) {
	nb, ok := reconcileBitmap(nil, cs*2, cs*2, map[int]uint64{0: 0xA})
	if ok || nb != nil {
		t.Fatal("expected (nil, false) when old is nil")
	}
}

// Case 8: present chunk with differing peer hash is cleared; neighbors kept.
func TestReconcileBitmap_DifferingHash(t *testing.T) {
	size := int64(cs * 3)
	old := buildOld(size, map[int]uint64{0: 0xAA, 1: 0xBB, 2: 0xCC})

	// Chunk 1 hash differs.
	peerHashes := map[int]uint64{0: 0xAA, 1: 0xFF, 2: 0xCC}

	nb, ok := reconcileBitmap(old, size, size, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !nb.Has(0) || !nb.Has(2) {
		t.Fatal("chunks 0 and 2 should be kept (hash matched)")
	}
	for c, want := range map[int]uint64{0: 0xAA, 2: 0xCC} {
		h, got := nb.Hash(c)
		if !got {
			t.Fatalf("chunk %d should carry hash in nb", c)
		}
		if h != want {
			t.Fatalf("chunk %d hash = %#x, want %#x", c, h, want)
		}
	}
	if nb.Has(1) {
		t.Fatal("chunk 1 should be absent (hash differed)")
	}
}

// Case 9: chunk absent in old (never downloaded) is absent in nb regardless of peer hash.
func TestReconcileBitmap_AbsentInOld(t *testing.T) {
	size := int64(cs * 3)
	// Only chunk 0 is set+hashed; chunks 1 and 2 are absent.
	old := buildOld(size, map[int]uint64{0: 0xAA})

	// Peer provides hashes for all, but only chunk 0 was present in old.
	peerHashes := map[int]uint64{0: 0xAA, 1: 0xBB, 2: 0xCC}

	nb, ok := reconcileBitmap(old, size, size, peerHashes)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !nb.Has(0) {
		t.Fatal("chunk 0 should be kept (present+matching)")
	}
	if nb.Has(1) || nb.Has(2) {
		t.Fatal("chunks 1 and 2 should be absent (not in old)")
	}
}
