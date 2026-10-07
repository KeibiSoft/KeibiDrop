// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"
)

func rec(hash uint64, stamp int64, dirty bool, baseHash uint64) ChunkRecord {
	return ChunkRecord{Hash: hash, Stamp: stamp, Dirty: dirty, BaseHash: baseHash, Present: true}
}

// The receiver's rule, case by case, as model v5 states it.
func TestJudgeChunk_Rules(t *testing.T) {
	cases := []struct {
		name             string
		own, in          ChunkRecord
		peerWinsTie      bool
		accept, conflict bool
	}{
		{"redelivery of the same bytes", rec(7, 10, true, 1), rec(7, 20, false, 7), true, false, false},
		{"newer stamp wins", rec(7, 10, false, 1), rec(8, 11, false, 7), false, true, false},
		{"older stamp loses", rec(7, 10, false, 1), rec(8, 9, false, 7), true, false, false},
		{"tie, rank says peer", rec(7, 10, false, 1), rec(8, 10, false, 7), true, true, false},
		{"tie, rank says us", rec(7, 10, false, 1), rec(8, 10, false, 7), false, false, false},
		{"dirty, base is our bytes: turn-taking", rec(7, 10, true, 1), rec(8, 11, false, 7), false, true, false},
		{"dirty, base never saw our bytes: conflict", rec(7, 10, true, 1), rec(8, 11, false, 6), false, true, true},
		{"clean, base never saw our bytes: adopt", rec(7, 10, false, 1), rec(8, 11, false, 6), false, true, false},
		{"no record yet: fresh create", ChunkRecord{}, rec(8, 1, false, 0), false, true, false},
		{"dirty but the incoming is older: ours stands", rec(7, 10, true, 1), rec(8, 9, false, 6), false, false, false},
	}
	for _, c := range cases {
		accept, conflict := JudgeChunk(c.own, c.in, c.peerWinsTie)
		require.Equal(t, c.accept, accept, "%s: accept", c.name)
		require.Equal(t, c.conflict, conflict, "%s: conflict", c.name)
	}
}

func TestChunkLedger_SizesAndRanges(t *testing.T) {
	require.Nil(t, NewChunkLedger(0))
	l := NewChunkLedger(10*LedgerUnit + 1)
	require.Equal(t, 11, l.Len())
	first, last := l.RecordRange(LedgerUnit-1, 2)
	require.Equal(t, 0, first)
	require.Equal(t, 1, last)
	first, last = l.RecordRange(10*LedgerUnit, 100*LedgerUnit)
	require.Equal(t, 10, first)
	require.Equal(t, 10, last, "clamped to the ledger")
	_, last = l.RecordRange(5, 0)
	require.Less(t, last, 0, "an empty range touches nothing")
}

// A write marks its records dirty and captures the base once; Settle gives
// them the identity; Clean hands them to the peer's judgment.
func TestChunkLedger_WriteSettleClean(t *testing.T) {
	l := NewChunkLedger(3 * LedgerUnit)
	base := ChunkRecord{Hash: 11, Stamp: 100, Present: true, Author: 1}
	l.MarkWritten(1, base)
	l.MarkWritten(1, ChunkRecord{Hash: 99, Stamp: 999}) // a second write keeps the first base
	r, ok := l.Record(1)
	require.True(t, ok)
	require.True(t, r.Dirty)
	require.Equal(t, uint64(11), r.BaseHash)
	require.Equal(t, int64(100), r.BaseStamp)
	require.True(t, l.AnyDirty())
	b, ok := l.DirtyBase()
	require.True(t, ok)
	require.Equal(t, int64(100), b)

	l.Settle(1, 42, 200)
	l.Settle(0, 1, 200) // not dirty: ignored
	r, _ = l.Record(1)
	require.Equal(t, uint64(42), r.Hash)
	require.Equal(t, int64(200), r.Stamp)
	require.True(t, r.Present)
	r0, _ := l.Record(0)
	require.Zero(t, r0.Stamp)
	require.Equal(t, int64(200), l.MaxDirtyStamp())

	l.Clean()
	require.False(t, l.AnyDirty())
	_, ok = l.DirtyBase()
	require.False(t, ok)
	r, _ = l.Record(1)
	require.Equal(t, uint64(42), r.Hash, "Clean keeps the record, only the dirty bit goes")
}

// A swap-save: equal hashes keep the target's record untouched (the false
// positive mints nothing); differing hashes take the new stamp with the
// target record as their base; the ledger takes the temp's length.
func TestChunkLedger_SettleSwap(t *testing.T) {
	target := NewChunkLedger(2 * LedgerUnit)
	target.recs[0] = ChunkRecord{Hash: 1, Stamp: 10, Author: 1, Present: true}
	target.recs[1] = ChunkRecord{Hash: 2, Stamp: 10, Author: 1, Present: true}
	temp := NewChunkLedger(3 * LedgerUnit)
	temp.recs[0] = ChunkRecord{Hash: 1, Present: true}
	temp.recs[1] = ChunkRecord{Hash: 22, Present: true}
	temp.recs[2] = ChunkRecord{Hash: 33, Present: true}

	changed := target.SettleSwap(temp, 50)
	require.Equal(t, []int{1, 2}, changed)
	require.Equal(t, 3, target.Len())
	r0, _ := target.Record(0)
	require.Equal(t, ChunkRecord{Hash: 1, Stamp: 10, Author: 1, Present: true}, r0, "equal bytes keep the record")
	r1, _ := target.Record(1)
	require.Equal(t, ChunkRecord{Hash: 22, Stamp: 50, BaseHash: 2, BaseStamp: 10, Present: true, Dirty: true}, r1)
	r2, _ := target.Record(2)
	require.Equal(t, ChunkRecord{Hash: 33, Stamp: 50, Present: true, Dirty: true}, r2, "a new chunk has no base")

	require.Nil(t, target.SettleSwap(nil, 60))
	same := NewChunkLedger(3 * LedgerUnit)
	copy(same.recs, target.recs)
	require.Empty(t, target.SettleSwap(same, 70), "an identical rewrite changes nothing")
}

func TestChunkLedger_DiffAndApply(t *testing.T) {
	l := NewChunkLedger(3 * LedgerUnit)
	l.recs[0] = rec(1, 10, false, 0)
	l.recs[1] = rec(2, 10, true, 0) // our unannounced change of chunk 1, from hash 0
	l.recs[2] = rec(3, 10, false, 0)
	peer := []ChunkRecord{
		{Hash: 1, Stamp: 10},               // same bytes
		{Hash: 20, Stamp: 11, BaseHash: 0}, // the peer changed chunk 1 from the same base: conflict
		{Hash: 30, Stamp: 11, BaseHash: 3}, // turn-taking on chunk 2
	}
	fetch, conflicts := l.Diff(peer, false)
	require.Equal(t, []int{1, 2}, fetch)
	require.Equal(t, []int{1}, conflicts)

	l.Apply(2, peer[2])
	r, _ := l.Record(2)
	require.Equal(t, uint64(30), r.Hash)
	require.False(t, r.Present, "accepted, not held yet")
	require.False(t, r.Dirty)
}

// The record identity is composed from the bitmap's chunk fingerprints and is
// unknown while any chunk of the record is absent.
func TestChunkLedger_SyncFromBitmap(t *testing.T) {
	size := int64(2*LedgerUnit + ChunkSize) // 17 bitmap chunks: 8, 8, 1
	bm := NewChunkBitmap(size)
	l := NewChunkLedger(size)
	per := LedgerUnit / ChunkSize
	for c := 0; c < per; c++ {
		bm.Set(c)
		bm.SetHash(c, uint64(100+c))
	}
	bm.Set(per) // record 1: one of eight chunks
	bm.SetHash(per, 7)
	bm.Set(2 * per) // record 2: its only chunk
	bm.SetHash(2*per, 9)
	l.MarkWritten(1, ChunkRecord{Hash: 5, Stamp: 3})

	l.syncFromBitmap(bm, 1000)
	r0, _ := l.Record(0)
	require.True(t, r0.Present)
	want := make([]byte, 0, per*8)
	for c := 0; c < per; c++ {
		want = binary.LittleEndian.AppendUint64(want, uint64(100+c))
	}
	require.Equal(t, xxh3.Hash(want), r0.Hash)
	require.Equal(t, int64(1000), r0.Stamp)
	require.Equal(t, uint8(1), r0.Author)

	r1, _ := l.Record(1)
	require.False(t, r1.Present, "seven of eight chunks are absent")
	require.True(t, r1.Dirty, "a sync never clears the dirty bit")
	require.Equal(t, uint64(5), r1.BaseHash)
	require.Zero(t, r1.Stamp, "a dirty record keeps its own stamp")

	r2, _ := l.Record(2)
	require.True(t, r2.Present, "the tail record spans one chunk")
	require.Equal(t, int64(1000), r2.Stamp)

	bm.Clear(0)
	l.syncFromBitmap(bm, 1000)
	r0, _ = l.Record(0)
	require.False(t, r0.Present)
	require.Zero(t, r0.Hash, "an absent clean record has no identity")
}

func TestChunkRecord_Codec(t *testing.T) {
	r := ChunkRecord{Hash: 1 << 63, Stamp: -5, Author: 1, BaseHash: 77, BaseStamp: 1234567890123, Present: true, Dirty: true}
	buf := appendRecord(nil, r)
	require.Len(t, buf, ledgerRecordSize)
	require.Equal(t, r, decodeRecord(buf))
	clean := ChunkRecord{Hash: 3}
	require.Equal(t, clean, decodeRecord(appendRecord(nil, clean)))
}
