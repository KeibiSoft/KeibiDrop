// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import "sync"

// ChunkSize is the granularity for tracking downloaded segments.
const ChunkSize = 512 * 1024 // 512 KiB

// StreamPoolSize is the number of parallel gRPC streams per file.
// Matches typical FUSE readahead parallelism on Linux.
const StreamPoolSize = 4

// ChunkBitmap tracks which chunks of a file have been downloaded.
// Thread-safe: Has() uses RLock, Set() uses Lock.
type ChunkBitmap struct {
	mu       sync.RWMutex
	bits     []uint64
	total    int
	have     int
	fileSize int64
}

// NewChunkBitmap creates a bitmap for a file of the given size.
// Returns nil for size <= 0 (empty files need no tracking).
func NewChunkBitmap(fileSize int64) *ChunkBitmap {
	if fileSize <= 0 {
		return nil
	}
	total := int((fileSize + ChunkSize - 1) / ChunkSize)
	numWords := (total + 63) / 64
	return &ChunkBitmap{
		bits:     make([]uint64, numWords),
		total:    total,
		have:     0,
		fileSize: fileSize,
	}
}

// Has returns true if the chunk at chunkIdx has been downloaded.
func (b *ChunkBitmap) Has(chunkIdx int) bool {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return false
	}
	word := chunkIdx / 64
	bit := uint(chunkIdx % 64)
	b.mu.RLock()
	v := b.bits[word] & (1 << bit)
	b.mu.RUnlock()
	return v != 0
}

// Set marks a chunk as downloaded. Idempotent.
func (b *ChunkBitmap) Set(chunkIdx int) {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return
	}
	word := chunkIdx / 64
	bit := uint(chunkIdx % 64)
	b.mu.Lock()
	if b.bits[word]&(1<<bit) == 0 {
		b.bits[word] |= 1 << bit
		b.have++
	}
	b.mu.Unlock()
}

// SetRange marks all chunks covering [offset, offset+size) as downloaded.
func (b *ChunkBitmap) SetRange(offset int64, size int) {
	if size <= 0 {
		return
	}
	startChunk := int(offset / ChunkSize)
	endChunk := int((offset + int64(size) - 1) / ChunkSize)
	for i := startChunk; i <= endChunk; i++ {
		b.Set(i)
	}
}

// HasRange returns true if all chunks covering [offset, offset+size) are downloaded.
func (b *ChunkBitmap) HasRange(offset int64, size int) bool {
	if size <= 0 {
		return true
	}
	startChunk := int(offset / ChunkSize)
	endChunk := int((offset + int64(size) - 1) / ChunkSize)
	for i := startChunk; i <= endChunk; i++ {
		if !b.Has(i) {
			return false
		}
	}
	return true
}

// IsComplete returns true if all chunks have been downloaded.
func (b *ChunkBitmap) IsComplete() bool {
	b.mu.RLock()
	v := b.have == b.total
	b.mu.RUnlock()
	return v
}

// Progress returns download completion as a fraction [0.0, 1.0].
func (b *ChunkBitmap) Progress() float64 {
	b.mu.RLock()
	h, t := b.have, b.total
	b.mu.RUnlock()
	if t == 0 {
		return 0
	}
	return float64(h) / float64(t)
}

// NextMissing returns the index of the first missing chunk starting from `from`.
// Returns -1 if no missing chunks remain from that point.
func (b *ChunkBitmap) NextMissing(from int) int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for i := from; i < b.total; i++ {
		word := i / 64
		bit := uint(i % 64)
		if b.bits[word]&(1<<bit) == 0 {
			return i
		}
	}
	return -1
}

// Total returns the total number of chunks.
func (b *ChunkBitmap) Total() int {
	return b.total
}

// FileSize returns the tracked file size.
func (b *ChunkBitmap) FileSize() int64 {
	return b.fileSize
}
