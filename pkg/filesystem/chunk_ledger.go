// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: The per-chunk version ledger of one file (CHUNK-LEDGER-DESIGN.md, stage 3):
// ABOUTME: one record per 4 MiB, the dirty set that survives a restart, the chunk judgment.

package filesystem

import (
	"encoding/binary"
	"sync"

	"github.com/zeebo/xxh3"
)

// LedgerUnit is the span of one record: eight bitmap chunks. Records at
// 512 KiB would cost 80 MB of sidecar for a 1 TB image; at 4 MiB it is
// 10 MB, and a 4 MiB record still keeps a 1 MiB patch to one fetch.
const LedgerUnit = 4 * 1024 * 1024

// ledgerRecordSize is the on-disk size of one record (sidecar v2).
const ledgerRecordSize = 8 + 8 + 8 + 8 + 1 + 1

// ChunkRecord is the version state of one ledger chunk.
type ChunkRecord struct {
	Hash      uint64 // identity of the bytes (recordHash); 0 while not Present
	Stamp     int64  // ns; above every stamp this peer had seen for the chunk when written
	Author    uint8  // 0 = this peer, 1 = the peer
	BaseHash  uint64 // the record this change started from: its hash, 0 = unknown
	BaseStamp int64  // and its stamp; 0 = fresh create, no prior content
	Present   bool   // the bytes are on disk: every bitmap chunk of the record
	Dirty     bool   // a local change the peer has not judged yet
}

// ChunkLedger owns the version state of one file, per chunk. The bitmap
// underneath keeps 512 KiB presence and fingerprints as today; syncFromBitmap
// lifts them into the records. A dirty record belongs to the write path: a
// sync refreshes its presence and hash, never its stamp, base or dirty bit.
type ChunkLedger struct {
	mu   sync.RWMutex
	size int64
	recs []ChunkRecord
}

// ledgerRecords is the number of records a file of size bytes needs.
func ledgerRecords(size int64) int {
	if size <= 0 {
		return 0
	}
	return int((size + LedgerUnit - 1) / LedgerUnit)
}

// NewChunkLedger makes an empty ledger for a file of size bytes; nil for an
// empty file, like NewChunkBitmap.
func NewChunkLedger(size int64) *ChunkLedger {
	if size <= 0 {
		return nil
	}
	return &ChunkLedger{size: size, recs: make([]ChunkRecord, ledgerRecords(size))}
}

// Len is the number of records.
func (l *ChunkLedger) Len() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.recs)
}

// Size is the file size the ledger describes.
func (l *ChunkLedger) Size() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.size
}

// Record returns record c; ok is false outside the ledger.
func (l *ChunkLedger) Record(c int) (rec ChunkRecord, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if c < 0 || c >= len(l.recs) {
		return ChunkRecord{}, false
	}
	return l.recs[c], true
}

// Records returns a copy of every record.
func (l *ChunkLedger) Records() []ChunkRecord {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ChunkRecord, len(l.recs))
	copy(out, l.recs)
	return out
}

// RecordRange returns the records a byte range touches, first to last
// inclusive; last < first for an empty range.
func (l *ChunkLedger) RecordRange(offset int64, n int) (first, last int) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if n <= 0 || offset < 0 || len(l.recs) == 0 {
		return 0, -1
	}
	first = int(offset / LedgerUnit)
	last = int((offset + int64(n) - 1) / LedgerUnit)
	if last >= len(l.recs) {
		last = len(l.recs) - 1
	}
	if first > last {
		return 0, -1
	}
	return first, last
}

// MarkWritten records a local write into chunk c. The first write of an edit
// session captures the base: the record the change started from. Presence
// and hash stay as the bitmap has them (syncFromBitmap).
func (l *ChunkLedger) MarkWritten(c int, base ChunkRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c < 0 || c >= len(l.recs) {
		return
	}
	r := &l.recs[c]
	if !r.Dirty {
		r.BaseHash = base.Hash
		r.BaseStamp = base.Stamp
		r.Stamp = 0
	}
	r.Dirty = true
	r.Author = 0
}

// Settle gives a dirty record its hash and stamp: Release (the identity the
// announce will carry) or the fill end. A zero hash keeps the current one.
func (l *ChunkLedger) Settle(c int, hash uint64, stamp int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c < 0 || c >= len(l.recs) || !l.recs[c].Dirty {
		return
	}
	r := &l.recs[c]
	if hash != 0 {
		r.Hash = hash
		r.Present = true
	}
	if stamp > r.Stamp {
		r.Stamp = stamp
	}
	r.Author = 0
}

// SettleSwap applies a rewritten working copy (temp) to this ledger at a
// swap-save: an equal hash keeps the target record, stamp and author
// included, so the swap-save false positive never mints a version; a
// differing hash takes stamp with the target record as its base. The ledger
// takes the temp's length. Returns the records that changed.
func (l *ChunkLedger) SettleSwap(temp *ChunkLedger, stamp int64) (changed []int) {
	if temp == nil {
		return nil
	}
	tr := temp.Records()
	tsize := temp.Size()
	l.mu.Lock()
	defer l.mu.Unlock()
	old := l.recs
	l.recs = make([]ChunkRecord, len(tr))
	l.size = tsize
	for c := range tr {
		if c < len(old) && old[c].Present && old[c].Hash == tr[c].Hash {
			l.recs[c] = old[c]
			continue
		}
		var prev ChunkRecord
		if c < len(old) {
			prev = old[c]
		}
		l.recs[c] = ChunkRecord{
			Hash:      tr[c].Hash,
			Stamp:     stamp,
			Author:    0,
			BaseHash:  prev.Hash,
			BaseStamp: prev.Stamp,
			Present:   tr[c].Present,
			Dirty:     true,
		}
		changed = append(changed, c)
	}
	return changed
}

// JudgeChunk is the receiver's rule for one chunk, pure. The same bytes
// (equal hash) are a redelivery and change nothing. A newer stamp wins; on
// an exact tie the fingerprint rank does (peerWinsTie). The judgment is a
// conflict when the receiver's record is a local change the incoming base
// never saw: dirty, and the incoming base is not the receiver's hash.
func JudgeChunk(own, in ChunkRecord, peerWinsTie bool) (accept, conflict bool) {
	if in.Hash == own.Hash {
		return false, false
	}
	accept = in.Stamp > own.Stamp || (in.Stamp == own.Stamp && peerWinsTie)
	if !accept {
		return false, false
	}
	conflict = own.Dirty && in.BaseHash != own.Hash
	return accept, conflict
}

// Judge applies JudgeChunk to record c. A chunk outside the ledger has no
// record: a fresh create, accepted and never a conflict.
func (l *ChunkLedger) Judge(c int, in ChunkRecord, peerWinsTie bool) (accept, conflict bool) {
	own, _ := l.Record(c)
	return JudgeChunk(own, in, peerWinsTie)
}

// Diff judges every incoming record: fetch lists the chunks whose accepted
// bytes this peer does not hold, conflicts the chunks whose local change the
// incoming base never saw.
func (l *ChunkLedger) Diff(peer []ChunkRecord, peerWinsTie bool) (fetch, conflicts []int) {
	for c, in := range peer {
		accept, conflict := l.Judge(c, in, peerWinsTie)
		if conflict {
			conflicts = append(conflicts, c)
		}
		if accept {
			fetch = append(fetch, c)
		}
	}
	return fetch, conflicts
}

// Apply adopts an accepted record: the bytes are not held yet, and any local
// change the judgment preserved elsewhere is no longer pending here.
func (l *ChunkLedger) Apply(c int, in ChunkRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c < 0 || c >= len(l.recs) {
		return
	}
	in.Present = false
	in.Dirty = false
	l.recs[c] = in
}

// Clean clears the dirty bit of every record: the announce went out and the
// peer judges from here.
func (l *ChunkLedger) Clean() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.recs {
		l.recs[c].Dirty = false
	}
}

// AnyDirty reports whether a local change is still unannounced.
func (l *ChunkLedger) AnyDirty() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, r := range l.recs {
		if r.Dirty {
			return true
		}
	}
	return false
}

// DirtyBase is the smallest base stamp among the dirty records, the
// conservative file-level base for the announce (it can only add a conflict
// copy, never hide one); ok is false without a dirty record.
func (l *ChunkLedger) DirtyBase() (base int64, ok bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, r := range l.recs {
		if !r.Dirty {
			continue
		}
		if !ok || r.BaseStamp < base {
			base = r.BaseStamp
			ok = true
		}
	}
	return base, ok
}

// MaxDirtyStamp is the newest stamp among the dirty records; 0 without one.
func (l *ChunkLedger) MaxDirtyStamp() int64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var m int64
	for _, r := range l.recs {
		if r.Dirty && r.Stamp > m {
			m = r.Stamp
		}
	}
	return m
}

// syncFromBitmap lifts presence and identity from the bitmap: a record is
// present when all its bitmap chunks are, and its hash is recordHash. A
// record that is not dirty carries the held version: heldStamp, the peer as
// author. Dirty records keep their stamp, base and dirty bit.
func (l *ChunkLedger) syncFromBitmap(bm *ChunkBitmap, heldStamp int64) {
	if bm == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.recs {
		r := &l.recs[c]
		hash, present := recordHash(bm, c)
		r.Present = present
		if present {
			r.Hash = hash
		} else if !r.Dirty {
			r.Hash = 0
		}
		if !r.Dirty {
			r.Author = 1
			if present {
				r.Stamp = heldStamp
			}
		}
	}
}

// recordHash is the identity of record c: xxh3 over the fingerprints of its
// bitmap chunks, in order. Composed, not read from disk, so a landing costs
// no second pass over the bytes; equal bytes give an equal identity. It is
// unknown while any chunk of the record is absent or unfingerprinted.
func recordHash(bm *ChunkBitmap, c int) (uint64, bool) {
	if bm == nil {
		return 0, false
	}
	per := LedgerUnit / bm.ChunkSizeBytes()
	if per <= 0 {
		return 0, false
	}
	first := c * per
	last := min(first+per, bm.Total())
	if first >= last {
		return 0, false
	}
	buf := make([]byte, 0, per*8)
	for i := first; i < last; i++ {
		h, ok := bm.Hash(i)
		if !ok {
			return 0, false
		}
		buf = binary.LittleEndian.AppendUint64(buf, h)
	}
	return xxh3.Hash(buf), true
}

// appendRecord encodes one record for the sidecar (v2), little-endian.
func appendRecord(buf []byte, r ChunkRecord) []byte {
	buf = binary.LittleEndian.AppendUint64(buf, r.Hash)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(r.Stamp)) // #nosec G115
	buf = binary.LittleEndian.AppendUint64(buf, r.BaseHash)
	buf = binary.LittleEndian.AppendUint64(buf, uint64(r.BaseStamp)) // #nosec G115
	buf = append(buf, r.Author)
	var flags byte
	if r.Present {
		flags |= 1
	}
	if r.Dirty {
		flags |= 2
	}
	return append(buf, flags)
}

// decodeRecord is the inverse of appendRecord; b holds at least ledgerRecordSize bytes.
func decodeRecord(b []byte) ChunkRecord {
	return ChunkRecord{
		Hash:      binary.LittleEndian.Uint64(b[0:]),
		Stamp:     int64(binary.LittleEndian.Uint64(b[8:])), // #nosec G115
		BaseHash:  binary.LittleEndian.Uint64(b[16:]),
		BaseStamp: int64(binary.LittleEndian.Uint64(b[24:])), // #nosec G115
		Author:    b[32],
		Present:   b[33]&1 != 0,
		Dirty:     b[33]&2 != 0,
	}
}

// basesFor returns, for each record a write touches that is not dirty yet,
// the record's current identity: the base the change starts from. Unknown
// (0) while a chunk of the record is still absent.
func (l *ChunkLedger) basesFor(bm *ChunkBitmap, offset int64, n int) map[int]uint64 {
	first, last := l.RecordRange(offset, n)
	out := make(map[int]uint64, last-first+1)
	for c := first; c <= last; c++ {
		if r, ok := l.Record(c); ok && !r.Dirty {
			h, _ := recordHash(bm, c)
			out[c] = h
		}
	}
	return out
}

// markRange marks the records a write touched dirty, with the bases basesFor
// captured before the bytes changed and the version stamp they belong to.
func (l *ChunkLedger) markRange(offset int64, n int, bases map[int]uint64, baseStamp int64) {
	first, last := l.RecordRange(offset, n)
	for c := first; c <= last; c++ {
		l.MarkWritten(c, ChunkRecord{Hash: bases[c], Stamp: baseStamp})
	}
}

// SettleDirty gives every dirty record the stamp the announce will carry
// (Release: the in-memory identity). Hashes follow from the bitmap at the
// next sync.
func (l *ChunkLedger) SettleDirty(stamp int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for c := range l.recs {
		if l.recs[c].Dirty && stamp > l.recs[c].Stamp {
			l.recs[c].Stamp = stamp
		}
	}
}
