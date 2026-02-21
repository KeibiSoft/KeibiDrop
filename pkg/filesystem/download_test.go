// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"math/rand"
	"sync"
	"testing"
)

func TestChunkBitmap_Basic(t *testing.T) {
	// 1 MiB file = 2 chunks of 512 KiB
	b := NewChunkBitmap(1024 * 1024)
	if b == nil {
		t.Fatal("expected non-nil bitmap")
	}
	if b.Total() != 2 {
		t.Fatalf("expected 2 chunks, got %d", b.Total())
	}
	if b.IsComplete() {
		t.Fatal("should not be complete initially")
	}
	if b.Progress() != 0 {
		t.Fatalf("expected 0 progress, got %f", b.Progress())
	}

	b.Set(0)
	if !b.Has(0) {
		t.Fatal("chunk 0 should be set")
	}
	if b.Has(1) {
		t.Fatal("chunk 1 should not be set")
	}
	if b.IsComplete() {
		t.Fatal("should not be complete with 1/2 chunks")
	}
	if b.Progress() != 0.5 {
		t.Fatalf("expected 0.5 progress, got %f", b.Progress())
	}

	b.Set(1)
	if !b.IsComplete() {
		t.Fatal("should be complete with 2/2 chunks")
	}
	if b.Progress() != 1.0 {
		t.Fatalf("expected 1.0 progress, got %f", b.Progress())
	}
}

func TestChunkBitmap_EmptyFile(t *testing.T) {
	b := NewChunkBitmap(0)
	if b != nil {
		t.Fatal("expected nil bitmap for empty file")
	}
}

func TestChunkBitmap_SingleChunk(t *testing.T) {
	// 100 bytes = 1 chunk
	b := NewChunkBitmap(100)
	if b.Total() != 1 {
		t.Fatalf("expected 1 chunk, got %d", b.Total())
	}
	b.Set(0)
	if !b.IsComplete() {
		t.Fatal("should be complete")
	}
}

func TestChunkBitmap_LargeFile(t *testing.T) {
	// 1 GiB = 2048 chunks
	b := NewChunkBitmap(1024 * 1024 * 1024)
	if b.Total() != 2048 {
		t.Fatalf("expected 2048 chunks, got %d", b.Total())
	}

	// Set all chunks
	for i := 0; i < 2048; i++ {
		b.Set(i)
	}
	if !b.IsComplete() {
		t.Fatal("should be complete")
	}
}

func TestChunkBitmap_SetRange(t *testing.T) {
	// 3 MiB file = 6 chunks
	b := NewChunkBitmap(3 * 1024 * 1024)
	if b.Total() != 6 {
		t.Fatalf("expected 6 chunks, got %d", b.Total())
	}

	// Read spanning chunks 1-2 (offset 512KiB, size 1MiB)
	b.SetRange(512*1024, 1024*1024)
	if !b.Has(1) {
		t.Fatal("chunk 1 should be set")
	}
	if !b.Has(2) {
		t.Fatal("chunk 2 should be set")
	}
	if b.Has(0) || b.Has(3) {
		t.Fatal("chunks 0 and 3 should not be set")
	}
}

func TestChunkBitmap_HasRange(t *testing.T) {
	b := NewChunkBitmap(3 * 1024 * 1024)
	b.Set(1)
	b.Set(2)

	// Range fully covered
	if !b.HasRange(512*1024, 1024*1024) {
		t.Fatal("range should be covered")
	}
	// Range partially covered (includes chunk 0 which is not set)
	if b.HasRange(0, 1024*1024) {
		t.Fatal("range should not be covered (chunk 0 missing)")
	}
}

func TestChunkBitmap_NextMissing(t *testing.T) {
	b := NewChunkBitmap(3 * 1024 * 1024)
	b.Set(0)
	b.Set(2)

	idx := b.NextMissing(0)
	if idx != 1 {
		t.Fatalf("expected next missing at 1, got %d", idx)
	}

	idx = b.NextMissing(2)
	if idx != 3 {
		t.Fatalf("expected next missing at 3, got %d", idx)
	}

	// Set remaining
	b.Set(1)
	b.Set(3)
	b.Set(4)
	b.Set(5)
	idx = b.NextMissing(0)
	if idx != -1 {
		t.Fatalf("expected -1 (all set), got %d", idx)
	}
}

func TestChunkBitmap_Idempotent(t *testing.T) {
	b := NewChunkBitmap(1024 * 1024)
	b.Set(0)
	b.Set(0) // Set same chunk again
	b.Set(0)
	if b.Progress() != 0.5 {
		t.Fatalf("expected 0.5 after idempotent sets, got %f", b.Progress())
	}
}

func TestChunkBitmap_ConcurrentAccess(t *testing.T) {
	// 15 MiB = 30 chunks — same as the .mov file
	b := NewChunkBitmap(15 * 1024 * 1024)

	var wg sync.WaitGroup
	// Simulate background prefetch (sequential)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < b.Total(); i++ {
			b.Set(i)
		}
	}()

	// Simulate QuickTime random reads (header + trailer pattern)
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Read header (chunk 0)
		b.Set(0)
		b.Has(0)
		// Read trailer (last 2 chunks)
		b.Set(b.Total() - 1)
		b.Set(b.Total() - 2)
		b.Has(b.Total() - 1)
	}()

	// Simulate random access reads
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(42))
		for i := 0; i < 100; i++ {
			idx := rng.Intn(b.Total())
			b.Set(idx)
			b.Has(idx)
			b.Progress()
			b.IsComplete()
		}
	}()

	wg.Wait()

	if !b.IsComplete() {
		t.Fatalf("expected complete after concurrent access, progress: %f", b.Progress())
	}
}

func TestChunkBitmap_QuickTimePattern(t *testing.T) {
	// Simulate QuickTime's read pattern on a 15 MiB .mov file:
	// Only reads header (first 4KB) and trailer (last ~13KB), skips middle.
	fileSize := int64(15 * 1024 * 1024) // 15 MiB
	b := NewChunkBitmap(fileSize)

	// QuickTime reads: offset=0, 4KB → chunk 0
	b.SetRange(0, 4096)

	// QuickTime reads: trailer near end
	b.SetRange(fileSize-13*1024, 13*1024)

	// Only 2 out of 30 chunks should be set (first and last).
	if b.IsComplete() {
		t.Fatal("should NOT be complete — QuickTime only reads header+trailer")
	}
	if b.Progress() >= 0.1 {
		t.Fatalf("expected low progress (<10%%), got %f", b.Progress())
	}

	// Now simulate background prefetch filling all gaps.
	for idx := 0; idx < b.Total(); idx++ {
		if !b.Has(idx) {
			b.Set(idx)
		}
	}

	if !b.IsComplete() {
		t.Fatal("should be complete after prefetch fills gaps")
	}
}
