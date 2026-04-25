// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync"
)

// .kdbitmap file format (binary, little-endian):
//
//	[magic 4B] [version 1B] [fileSize 8B] [total 4B] [have 4B] [chunkSize 4B] [bits N*8B]
//
// Each field:
const (
	bitmapMagicSize     = 4
	bitmapVersionSize   = 1
	bitmapFileSizeSize  = 8
	bitmapTotalSize     = 4
	bitmapHaveSize      = 4
	bitmapChunkSizeSize = 4
	bitmapHeaderSize    = bitmapMagicSize + bitmapVersionSize + bitmapFileSizeSize + bitmapTotalSize + bitmapHaveSize + bitmapChunkSizeSize // 25

	// Header field offsets:
	offMagic     = 0
	offVersion   = offMagic + bitmapMagicSize       // 4
	offFileSize  = offVersion + bitmapVersionSize   // 5
	offTotal     = offFileSize + bitmapFileSizeSize // 13
	offHave      = offTotal + bitmapTotalSize       // 17
	offChunkSize = offHave + bitmapHaveSize         // 21

	bitmapVersion = 1
)

var bitmapMagic = [4]byte{'K', 'D', 'B', 'M'}

// ChunkSize is the granularity for tracking downloaded segments.
const ChunkSize = 512 * 1024 // 512 KiB

// StreamPoolSize is the number of parallel gRPC streams per file.
// Matches typical FUSE readahead parallelism on Linux.
const StreamPoolSize = 4

// ChunkBitmap tracks which chunks of a file have been downloaded.
// Thread-safe: Has() uses RLock, Set() uses Lock.
type ChunkBitmap struct {
	mu        sync.RWMutex
	bits      []uint64
	total     int
	have      int
	fileSize  int64
	chunkSize int
}

// NewChunkBitmap creates a bitmap for a file of the given size using ChunkSize granularity.
// Returns nil for size <= 0 (empty files need no tracking).
func NewChunkBitmap(fileSize int64) *ChunkBitmap {
	return NewChunkBitmapWithSize(fileSize, ChunkSize)
}

// NewChunkBitmapWithSize creates a bitmap with a custom chunk size.
func NewChunkBitmapWithSize(fileSize int64, chunkSize int) *ChunkBitmap {
	if fileSize <= 0 {
		return nil
	}
	total := int((fileSize + int64(chunkSize) - 1) / int64(chunkSize))
	numWords := (total + 63) / 64
	return &ChunkBitmap{
		bits:      make([]uint64, numWords),
		total:     total,
		have:      0,
		fileSize:  fileSize,
		chunkSize: chunkSize,
	}
}

// Has returns true if the chunk at chunkIdx has been downloaded.
func (b *ChunkBitmap) Has(chunkIdx int) bool {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return false
	}
	word := chunkIdx / 64
	bit := uint(chunkIdx % 64) // #nosec G115
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
	bit := uint(chunkIdx % 64) // #nosec G115
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
	cs := int64(b.chunkSize)
	startChunk := int(offset / cs)
	endChunk := int((offset + int64(size) - 1) / cs)
	for i := startChunk; i <= endChunk; i++ {
		b.Set(i)
	}
}

// HasRange returns true if all chunks covering [offset, offset+size) are downloaded.
func (b *ChunkBitmap) HasRange(offset int64, size int) bool {
	if size <= 0 {
		return true
	}
	cs := int64(b.chunkSize)
	startChunk := int(offset / cs)
	endChunk := int((offset + int64(size) - 1) / cs)
	for i := startChunk; i <= endChunk; i++ {
		if !b.Has(i) {
			return false
		}
	}
	return true
}

// ChunkSizeBytes returns the chunk size used by this bitmap.
func (b *ChunkBitmap) ChunkSizeBytes() int {
	return b.chunkSize
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
		bit := uint(i % 64) // #nosec G115
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

// Have returns the number of downloaded chunks.
func (b *ChunkBitmap) Have() int {
	b.mu.RLock()
	h := b.have
	b.mu.RUnlock()
	return h
}

// FileSize returns the tracked file size.
func (b *ChunkBitmap) FileSize() int64 {
	return b.fileSize
}

// BitmapPath returns the .kdbitmap sidecar path for a given file path.
func BitmapPath(filePath string) string {
	return filePath + ".kdbitmap"
}

// Save writes the bitmap state to a .kdbitmap file.
func (b *ChunkBitmap) Save(path string) error {
	b.mu.RLock()
	defer b.mu.RUnlock()

	buf := make([]byte, bitmapHeaderSize+len(b.bits)*8)
	copy(buf[offMagic:], bitmapMagic[:])
	buf[offVersion] = bitmapVersion
	binary.LittleEndian.PutUint64(buf[offFileSize:], uint64(b.fileSize)) // #nosec G115
	binary.LittleEndian.PutUint32(buf[offTotal:], uint32(b.total))
	binary.LittleEndian.PutUint32(buf[offHave:], uint32(b.have))
	binary.LittleEndian.PutUint32(buf[offChunkSize:], uint32(b.chunkSize))
	for i, w := range b.bits {
		binary.LittleEndian.PutUint64(buf[bitmapHeaderSize+i*8:], w)
	}
	return os.WriteFile(path, buf, 0600)
}

// LoadChunkBitmap reads a bitmap from a .kdbitmap file.
// Returns an error if the file is corrupt or the fileSize doesn't match.
func LoadChunkBitmap(path string, expectedFileSize int64) (*ChunkBitmap, error) {
	data, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, err
	}
	if len(data) < bitmapHeaderSize {
		return nil, fmt.Errorf("bitmap file too short: %d bytes", len(data))
	}
	if [4]byte(data[offMagic:offMagic+4]) != bitmapMagic {
		return nil, fmt.Errorf("invalid bitmap magic")
	}
	if data[offVersion] != bitmapVersion {
		return nil, fmt.Errorf("unsupported bitmap version: %d", data[offVersion])
	}
	fileSize := int64(binary.LittleEndian.Uint64(data[offFileSize:])) // #nosec G115
	if fileSize != expectedFileSize {
		return nil, fmt.Errorf("bitmap fileSize mismatch: got %d, expected %d", fileSize, expectedFileSize)
	}
	total := int(binary.LittleEndian.Uint32(data[offTotal:]))
	have := int(binary.LittleEndian.Uint32(data[offHave:]))
	chunkSize := int(binary.LittleEndian.Uint32(data[offChunkSize:]))
	if chunkSize == 0 {
		chunkSize = ChunkSize // backwards compat
	}

	numWords := (total + 63) / 64
	if len(data) < bitmapHeaderSize+numWords*8 {
		return nil, fmt.Errorf("bitmap data truncated: need %d bytes, have %d", bitmapHeaderSize+numWords*8, len(data))
	}

	bits := make([]uint64, numWords)
	for i := range bits {
		bits[i] = binary.LittleEndian.Uint64(data[bitmapHeaderSize+i*8:])
	}
	return &ChunkBitmap{
		bits:      bits,
		total:     total,
		have:      have,
		fileSize:  fileSize,
		chunkSize: chunkSize,
	}, nil
}
