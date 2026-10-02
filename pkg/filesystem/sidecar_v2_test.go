// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// A v2 sidecar round-trips the bitmap, its fingerprints, the held version,
// the edit base and every record; a v1 sidecar still loads with no metadata.
func TestSidecarV2_RoundTrip(t *testing.T) {
	size := int64(2*LedgerUnit + 3*ChunkSize)
	bm := NewChunkBitmap(size)
	for _, c := range []int{0, 1, 2, 9, 18} {
		bm.Set(c)
		bm.SetHash(c, uint64(1000+c))
	}
	l := NewChunkLedger(size)
	l.MarkWritten(1, ChunkRecord{Hash: 55, Stamp: 500})
	l.Settle(1, 66, 700)
	l.syncFromBitmap(bm, 600)
	meta := &SidecarMeta{HeldStamp: 600, EditBase: 500, Ledger: l}

	p := BitmapPath(filepath.Join(t.TempDir(), "f.bin"))
	require.NoError(t, SaveSidecar(p, bm, meta))
	left, _ := filepath.Glob(p + ".*.tmp")
	require.Empty(t, left, "the temp file is renamed over")

	got, gotMeta, err := LoadSidecar(p, size)
	require.NoError(t, err)
	require.Equal(t, bm.Have(), got.Have())
	require.Equal(t, bm.Total(), got.Total())
	for _, c := range []int{0, 1, 2, 9, 18} {
		require.True(t, got.Has(c))
		h, ok := got.Hash(c)
		require.True(t, ok)
		require.Equal(t, uint64(1000+c), h)
	}
	require.False(t, got.Has(3))
	_, ok := got.Hash(3)
	require.False(t, ok, "no fingerprint for an absent chunk")
	require.NotNil(t, gotMeta)
	require.Equal(t, int64(600), gotMeta.HeldStamp)
	require.Equal(t, int64(500), gotMeta.EditBase)
	require.NotNil(t, gotMeta.Ledger)
	require.Equal(t, l.Records(), gotMeta.Ledger.Records())
	require.True(t, gotMeta.Ledger.AnyDirty())

	// The plain loader reads the same file.
	bm2, err := LoadChunkBitmap(p, size)
	require.NoError(t, err)
	require.True(t, bm2.HasHashes())

	// v1 from Save: bitmap only, no metadata, no fingerprints.
	p1 := BitmapPath(filepath.Join(t.TempDir(), "v1.bin"))
	require.NoError(t, bm.Save(p1))
	got1, meta1, err := LoadSidecar(p1, size)
	require.NoError(t, err)
	require.Nil(t, meta1)
	require.Equal(t, bm.Have(), got1.Have())
	require.False(t, got1.HasHashes())
}

// A v2 sidecar without fingerprints or records loads as a bitmap plus the
// held version; nothing else is invented.
func TestSidecarV2_NoHashesNoLedger(t *testing.T) {
	size := int64(3 * ChunkSize)
	bm := NewChunkBitmap(size)
	bm.Set(1)
	p := BitmapPath(filepath.Join(t.TempDir(), "f.bin"))
	require.NoError(t, SaveSidecar(p, bm, &SidecarMeta{HeldStamp: 42}))
	got, meta, err := LoadSidecar(p, size)
	require.NoError(t, err)
	require.True(t, got.Has(1))
	require.False(t, got.HasHashes())
	require.NotNil(t, meta)
	require.Equal(t, int64(42), meta.HeldStamp)
	require.Nil(t, meta.Ledger)
}

// Corrupt or mismatched sidecars are errors, never a partial state.
func TestSidecarV2_Rejects(t *testing.T) {
	size := int64(LedgerUnit + ChunkSize)
	bm := NewChunkBitmap(size)
	bm.Set(0)
	bm.SetHash(0, 9)
	l := NewChunkLedger(size)
	dir := t.TempDir()
	p := BitmapPath(filepath.Join(dir, "f.bin"))
	require.NoError(t, SaveSidecar(p, bm, &SidecarMeta{HeldStamp: 1, Ledger: l}))

	_, _, err := LoadSidecar(p, size+1)
	require.Error(t, err, "size mismatch")

	data, err := os.ReadFile(p)
	require.NoError(t, err)
	for _, cut := range []int{len(data) - 1, len(data) - ledgerRecordSize - 5, bitmapHeaderSize + 8 + 3} {
		q := filepath.Join(dir, "cut.kdbitmap")
		require.NoError(t, os.WriteFile(q, data[:cut], 0o600))
		_, _, err = LoadSidecar(q, size)
		require.Error(t, err, "truncated at %d of %d", cut, len(data))
	}

	bad := append([]byte(nil), data...)
	bad[offVersion] = 9
	q := filepath.Join(dir, "ver.kdbitmap")
	require.NoError(t, os.WriteFile(q, bad, 0o600))
	_, _, err = LoadSidecar(q, size)
	require.Error(t, err, "unknown version")
}
