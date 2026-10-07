// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
)

// .kdbitmap file format (binary, little-endian):
//
//	[magic 4B] [version 1B] [fileSize 8B] [total 4B] [have 4B] [chunkSize 4B] [bits N*8B]
//
// Field sizes:
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

	bitmapVersion  = 1
	bitmapVersion2 = 2 // v1 layout, then the fingerprints and the ledger (SaveSidecar)
)

var bitmapMagic = [4]byte{'K', 'D', 'B', 'M'}

// ChunkSize is the granularity for tracking downloaded segments.
const ChunkSize = 512 * 1024 // 512 KiB

// StreamPoolSize is the number of parallel gRPC streams per file.
// Matches typical FUSE readahead parallelism on Linux.
const StreamPoolSize = 4

// poolSizeForFile bounds the pool to the blocks the file spans. ReadAt
// shards by 16 MiB block, so a file under 16 MiB only uses stream 0 and
// extra streams are setup and teardown cost. Unknown size keeps the full
// pool.
func poolSizeForFile(size int64) int {
	if size <= 0 {
		return StreamPoolSize
	}
	n := int((size + ReadAheadBlock - 1) / ReadAheadBlock)
	return min(max(n, 1), StreamPoolSize)
}

// ReadAheadBlock is the amount one on-demand read miss fetches per round trip.
// The gRPC frame size caps it, so the block lands in a single message. The
// whole block is cached, so later reads inside it are local. This turns ~32
// round trips per 16 MiB into one.
const ReadAheadBlock = config.GRPCStreamBuffer // 16 MiB

// ProbeFetch is what one cache miss fetches when the read is not part of a
// proven sequential stream: the first touch of a file, a thumbnailer, an NLE's
// filmstrip probes, a seek. Measured 2026-09-09: twenty single-frame probes
// across a cold 82 MB clip pulled all 82 MB at ReadAheadBlock granularity. A
// stream that has consumed half a block (the read-ahead gate) is back on whole
// blocks. Read-ahead off (window 0) keeps the plain block fetch.
const ProbeFetch = 2 * 1024 * 1024

// SlowFetchNotice is how long one demand fetch may hold a reader before the
// filesystem reports it through FS.OnSlowFetch. A probe unit lands in well
// under a second on a lane that is not shared; a wait this long is what the
// free relay lane feels like under load, and the session says so once.
const SlowFetchNotice = 2 * time.Second

// SmallFileWarmThreshold marks files the sibling warmer batches. It stays
// below the ReadBatch frame cap, so a warmed file arrives as one frame.
const SmallFileWarmThreshold = 256 * 1024

// WarmBatchBudget bounds the bytes one warm batch requests.
const WarmBatchBudget = 4 * 1024 * 1024

// WarmBatchMaxFiles matches the server's ReadBatch path cap.
const WarmBatchMaxFiles = 512

// readAheadWindowBlocks converts a read-ahead window in MB (config
// read_ahead_window_mb) to a count of ReadAheadBlock-sized blocks. A positive
// sub-block value rounds up to one block. <=0 turns read-ahead off.
func readAheadWindowBlocks(mb int) int {
	if mb <= 0 {
		return 0
	}
	blocks := mb * 1024 * 1024 / ReadAheadBlock
	if blocks < 1 {
		blocks = 1
	}
	return blocks
}

// ChunkBitmap tracks which chunks of a file are downloaded.
// Thread-safe: Has() uses RLock, Set() uses Lock.
// hashes stays nil until the first SetHash call (no fingerprints, e.g. fresh load).
type ChunkBitmap struct {
	mu        sync.RWMutex
	bits      []uint64
	hashes    []uint64 // Per-chunk xxh3-64 fingerprints. nil means no fingerprints.
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

// Has reports whether the chunk at chunkIdx is downloaded.
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

// HasRange reports whether all chunks covering [offset, offset+size) are downloaded.
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

// SetHash stores a per-chunk content fingerprint. Lazily allocates hashes on first use.
func (b *ChunkBitmap) SetHash(chunkIdx int, h uint64) {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return
	}
	b.mu.Lock()
	if b.hashes == nil {
		b.hashes = make([]uint64, b.total)
	}
	b.hashes[chunkIdx] = h
	b.mu.Unlock()
}

// Hash returns the stored fingerprint for a chunk. It returns (0, false) when
// hashes is nil, the chunk index is out of range, or the chunk is absent.
// A zero hash value does not mean absent; only the bit and the alloc matter.
func (b *ChunkBitmap) Hash(chunkIdx int) (uint64, bool) {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return 0, false
	}
	word := chunkIdx / 64
	bit := uint(chunkIdx % 64) // #nosec G115
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.hashes == nil {
		return 0, false
	}
	if b.bits[word]&(1<<bit) == 0 {
		return 0, false
	}
	return b.hashes[chunkIdx], true
}

// Clear clears the bit for chunkIdx and decrements have. Zeroes hashes[chunkIdx]
// if hashes is allocated. Idempotent for already-clear chunks.
func (b *ChunkBitmap) Clear(chunkIdx int) {
	if chunkIdx < 0 || chunkIdx >= b.total {
		return
	}
	word := chunkIdx / 64
	bit := uint(chunkIdx % 64) // #nosec G115
	b.mu.Lock()
	if b.bits[word]&(1<<bit) != 0 {
		b.bits[word] &^= 1 << bit
		b.have--
	}
	if b.hashes != nil {
		b.hashes[chunkIdx] = 0
	}
	b.mu.Unlock()
}

// ClearRange clears all chunks covering [offset, offset+size). Mirrors SetRange.
func (b *ChunkBitmap) ClearRange(offset int64, size int) {
	if size <= 0 {
		return
	}
	cs := int64(b.chunkSize)
	startChunk := int(offset / cs)
	endChunk := int((offset + int64(size) - 1) / cs)
	for i := startChunk; i <= endChunk; i++ {
		b.Clear(i)
	}
}

// ChunkSizeBytes returns the chunk size used by this bitmap.
func (b *ChunkBitmap) ChunkSizeBytes() int {
	return b.chunkSize
}

// HasHashes reports whether the bitmap stores per-chunk fingerprints.
// A fresh or disk-loaded bitmap has none until the first SetHash call.
func (b *ChunkBitmap) HasHashes() bool {
	b.mu.RLock()
	v := b.hashes != nil
	b.mu.RUnlock()
	return v
}

// IsComplete reports whether all chunks are downloaded.
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

// NextMissing returns the index of the first missing chunk at or after from.
// It returns -1 when no missing chunks remain.
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
	return writeFileAtomic(path, buf)
}

// writeFileAtomic writes buf to path through a temp file renamed over it.
// Whole file or nothing: a crash mid-write must not leave a truncated sidecar
// that reads as no sidecar, and a reader must never see a half-written one.
// The temp name is unique per writer: the timer flush and the fill's save
// run at the same time, and one shared name let a writer truncate the file
// the other was renaming into place (a 0-byte sidecar, 2026-10-02).
func writeFileAtomic(path string, buf []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := RenameShared(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// SidecarMeta is what a v2 sidecar carries beyond the bitmap
// (CHUNK-LEDGER-DESIGN.md 3.3): the version the present bytes belong to and
// the ledger, whose dirty records are an unannounced local change that must
// survive a daemon restart.
type SidecarMeta struct {
	HeldStamp int64        // HeldMtimeNs: the peer version the present bytes came from
	EditBase  int64        // EditBaseMtimeNs of the unannounced edit; 0 without one
	Ledger    *ChunkLedger // the records; nil when the sidecar holds none
}

// SaveSidecar writes a v2 sidecar: the v1 layout, then the fingerprints, then
// the ledger. Whole file or nothing, like Save. Save keeps writing v1 for the
// pull paths that carry no ledger; both versions load.
func SaveSidecar(path string, bm *ChunkBitmap, meta *SidecarMeta) error {
	if bm == nil {
		return fmt.Errorf("no bitmap to save")
	}
	bm.mu.RLock()
	buf := make([]byte, bitmapHeaderSize, bitmapHeaderSize+len(bm.bits)*8+1+len(bm.hashes)*8+24)
	copy(buf[offMagic:], bitmapMagic[:])
	buf[offVersion] = bitmapVersion2
	binary.LittleEndian.PutUint64(buf[offFileSize:], uint64(bm.fileSize))   // #nosec G115
	binary.LittleEndian.PutUint32(buf[offTotal:], uint32(bm.total))         // #nosec G115
	binary.LittleEndian.PutUint32(buf[offHave:], uint32(bm.have))           // #nosec G115
	binary.LittleEndian.PutUint32(buf[offChunkSize:], uint32(bm.chunkSize)) // #nosec G115
	for _, w := range bm.bits {
		buf = binary.LittleEndian.AppendUint64(buf, w)
	}
	if bm.hashes == nil {
		buf = append(buf, 0)
	} else {
		buf = append(buf, 1)
		for _, h := range bm.hashes {
			buf = binary.LittleEndian.AppendUint64(buf, h)
		}
	}
	bm.mu.RUnlock()

	var recs []ChunkRecord
	var held, base int64
	if meta != nil {
		held, base = meta.HeldStamp, meta.EditBase
		if meta.Ledger != nil {
			recs = meta.Ledger.Records()
		}
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(LedgerUnit))
	buf = binary.LittleEndian.AppendUint64(buf, uint64(held))      // #nosec G115
	buf = binary.LittleEndian.AppendUint64(buf, uint64(base))      // #nosec G115
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(recs))) // #nosec G115
	for _, r := range recs {
		buf = appendRecord(buf, r)
	}
	return writeFileAtomic(path, buf)
}

// LoadChunkBitmap reads the bitmap of a v1 or v2 sidecar.
// It returns an error when the file is corrupt or the fileSize does not match.
func LoadChunkBitmap(path string, expectedFileSize int64) (*ChunkBitmap, error) {
	bm, _, err := LoadSidecar(path, expectedFileSize)
	return bm, err
}

// readShared reads a whole file through OpenShared. On Windows a plain open
// passes no delete sharing, so a writer renaming a new version over the file
// (writeFileAtomic) makes it fail with a sharing violation.
func readShared(path string) ([]byte, error) {
	f, err := OpenShared(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// LoadSidecar reads a sidecar: the bitmap, and for v2 its fingerprints and
// the ledger (meta is nil for v1). A truncated or mismatched file is an
// error, never a partial state.
func LoadSidecar(path string, expectedFileSize int64) (*ChunkBitmap, *SidecarMeta, error) {
	data, err := readShared(path)
	if err != nil {
		return nil, nil, err
	}
	if len(data) < bitmapHeaderSize {
		return nil, nil, fmt.Errorf("bitmap file too short: %d bytes", len(data))
	}
	if [4]byte(data[offMagic:offMagic+4]) != bitmapMagic {
		return nil, nil, fmt.Errorf("invalid bitmap magic")
	}
	version := data[offVersion]
	if version != bitmapVersion && version != bitmapVersion2 {
		return nil, nil, fmt.Errorf("unsupported bitmap version: %d", version)
	}
	fileSize := int64(binary.LittleEndian.Uint64(data[offFileSize:])) // #nosec G115
	if fileSize != expectedFileSize {
		return nil, nil, fmt.Errorf("bitmap fileSize mismatch: got %d, expected %d", fileSize, expectedFileSize)
	}
	total := int(binary.LittleEndian.Uint32(data[offTotal:]))
	have := int(binary.LittleEndian.Uint32(data[offHave:]))
	chunkSize := int(binary.LittleEndian.Uint32(data[offChunkSize:]))
	if chunkSize == 0 {
		chunkSize = ChunkSize // Backward compatibility: old files lack chunkSize.
	}

	numWords := (total + 63) / 64
	pos := bitmapHeaderSize + numWords*8
	if len(data) < pos {
		return nil, nil, fmt.Errorf("bitmap data truncated: need %d bytes, have %d", pos, len(data))
	}
	bits := make([]uint64, numWords)
	for i := range bits {
		bits[i] = binary.LittleEndian.Uint64(data[bitmapHeaderSize+i*8:])
	}
	bm := &ChunkBitmap{
		bits:      bits,
		total:     total,
		have:      have,
		fileSize:  fileSize,
		chunkSize: chunkSize,
	}
	if version == bitmapVersion {
		return bm, nil, nil
	}

	need := func(n int) error {
		if len(data) < pos+n {
			return fmt.Errorf("sidecar truncated: need %d bytes, have %d", pos+n, len(data))
		}
		return nil
	}
	if err := need(1); err != nil {
		return nil, nil, err
	}
	hasHashes := data[pos]
	pos++
	if hasHashes == 1 {
		if err := need(total * 8); err != nil {
			return nil, nil, err
		}
		bm.hashes = make([]uint64, total)
		for i := range bm.hashes {
			bm.hashes[i] = binary.LittleEndian.Uint64(data[pos+i*8:])
		}
		pos += total * 8
	}
	if err := need(24); err != nil {
		return nil, nil, err
	}
	unit := int(binary.LittleEndian.Uint32(data[pos:]))
	held := int64(binary.LittleEndian.Uint64(data[pos+4:]))  // #nosec G115
	base := int64(binary.LittleEndian.Uint64(data[pos+12:])) // #nosec G115
	nrec := int(binary.LittleEndian.Uint32(data[pos+20:]))
	pos += 24
	meta := &SidecarMeta{HeldStamp: held, EditBase: base}
	if nrec == 0 {
		return bm, meta, nil
	}
	if unit != LedgerUnit || nrec != ledgerRecords(fileSize) {
		return nil, nil, fmt.Errorf("sidecar ledger mismatch: unit %d, %d records for %d bytes", unit, nrec, fileSize)
	}
	if err := need(nrec * ledgerRecordSize); err != nil {
		return nil, nil, err
	}
	l := NewChunkLedger(fileSize)
	for i := range l.recs {
		l.recs[i] = decodeRecord(data[pos+i*ledgerRecordSize:])
	}
	meta.Ledger = l
	return bm, meta, nil
}
