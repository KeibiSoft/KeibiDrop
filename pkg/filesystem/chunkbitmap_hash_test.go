// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: tests for ChunkBitmap per-chunk hashes and clear API.
// ABOUTME: covers SetHash, Hash, Clear, ClearRange, and persistence round-trip.

package filesystem

import (
	"os"
	"testing"
)

func TestChunkBitmap_SetHash_Present(t *testing.T) {
	// 1 MiB = 2 chunks; set chunk 0 then assign a hash.
	b := NewChunkBitmap(1024 * 1024)
	b.Set(0)
	b.SetHash(0, 0xdeadbeefcafe1234)
	h, ok := b.Hash(0)
	if !ok {
		t.Fatal("expected hash present for set chunk")
	}
	if h != 0xdeadbeefcafe1234 {
		t.Fatalf("expected 0xdeadbeefcafe1234, got %x", h)
	}
}

func TestChunkBitmap_Hash_FreshBitmap(t *testing.T) {
	b := NewChunkBitmap(1024 * 1024)
	b.Set(0)
	// No SetHash called: Hash should return false.
	_, ok := b.Hash(0)
	if ok {
		t.Fatal("expected false for fresh bitmap with no hashes set")
	}
}

func TestChunkBitmap_Hash_PersistenceRoundTrip(t *testing.T) {
	// 3 MiB = 6 chunks; set and hash chunks 1 and 3, then round-trip.
	b := NewChunkBitmap(3 * 1024 * 1024)
	b.Set(1)
	b.Set(3)
	b.SetHash(1, 0x1111111111111111)
	b.SetHash(3, 0x3333333333333333)

	tmp, err := os.CreateTemp("", "*.kdbitmap")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	defer os.Remove(tmp.Name())

	if err := b.Save(tmp.Name()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadChunkBitmap(tmp.Name(), 3*1024*1024)
	if err != nil {
		t.Fatalf("LoadChunkBitmap: %v", err)
	}

	// Bits, have, and fileSize must survive.
	if loaded.Have() != 2 {
		t.Fatalf("expected have=2, got %d", loaded.Have())
	}
	if !loaded.Has(1) || !loaded.Has(3) {
		t.Fatal("expected chunks 1 and 3 to be set after load")
	}
	if loaded.FileSize() != 3*1024*1024 {
		t.Fatalf("expected fileSize 3 MiB, got %d", loaded.FileSize())
	}

	// Hashes must NOT be persisted (nil hashes after load, Hash returns false).
	for i := 0; i < loaded.Total(); i++ {
		if _, ok := loaded.Hash(i); ok {
			t.Fatalf("expected no hash for chunk %d after load", i)
		}
	}
}

func TestChunkBitmap_Clear(t *testing.T) {
	// 2 MiB = 4 chunks; set chunk 2 with a hash, then clear it.
	b := NewChunkBitmap(2 * 1024 * 1024)
	b.Set(2)
	b.SetHash(2, 0xabcdef0123456789)

	haveBefore := b.Have()
	b.Clear(2)

	if b.Has(2) {
		t.Fatal("chunk 2 should not be set after Clear")
	}
	if b.Have() != haveBefore-1 {
		t.Fatalf("Have should decrement: want %d, got %d", haveBefore-1, b.Have())
	}
	if _, ok := b.Hash(2); ok {
		t.Fatal("Hash should return false after Clear")
	}
}

func TestChunkBitmap_ClearRange(t *testing.T) {
	// 4 MiB = 8 chunks; set all, then clear chunks 2-4.
	// offset=1MiB (start of chunk 2), size=1.5MiB (covers chunks 2,3,4).
	b := NewChunkBitmap(4 * 1024 * 1024)
	for i := 0; i < b.Total(); i++ {
		b.Set(i)
	}

	const oneMiB = 1024 * 1024
	b.ClearRange(oneMiB, oneMiB+oneMiB/2) // offset=1MiB, size=1.5MiB => chunks 2,3,4

	// Inside range: cleared.
	for _, idx := range []int{2, 3, 4} {
		if b.Has(idx) {
			t.Fatalf("chunk %d should be cleared", idx)
		}
	}
	// Outside range: untouched.
	for _, idx := range []int{1, 5} {
		if !b.Has(idx) {
			t.Fatalf("chunk %d should still be set", idx)
		}
	}
}
