//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zeebo/xxh3"
)

// mockHashProvider stands in for the peer's GetChunkHashes RPC.
type mockHashProvider struct {
	size   int64
	hashes []byte
	err    error
}

func (m *mockHashProvider) GetChunkHashes(_ context.Context, _ string, _ int64) (int64, []byte, error) {
	return m.size, m.hashes, m.err
}

// h16 is the 16-byte xxh3-128 digest of b (one chunk's worth).
func h16(b []byte) []byte { d := xxh3.Hash128(b).Bytes(); return d[:] }

// chunk builds a ChunkSize-byte block filled with a marker byte.
func chunk(marker byte) []byte { return bytes.Repeat([]byte{marker}, ChunkSize) }

func writeCacheFile(t *testing.T, content []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func runDiff(t *testing.T, oldBitmap *ChunkBitmap, cachePath string, prov chunkHashProvider, newSize int64) *ChunkBitmap {
	t.Helper()
	nb := NewChunkBitmap(newSize)
	d := &Dir{FsCtx: context.Background(), logger: slog.Default()}
	d.deltaRemarkUnchanged("/f.bin", cachePath, oldBitmap, nb, prov, newSize)
	return nb
}

func bitmapHaving(chunks int, have ...int) *ChunkBitmap {
	b := NewChunkBitmap(int64(chunks) * int64(ChunkSize))
	for _, i := range have {
		b.Set(i)
	}
	return b
}

// Region edit: chunk 1 changed, 0 and 2 identical. Only the changed chunk is
// re-fetched; the unchanged ones are kept from cache.
func TestEditDiff_RegionEditKeepsUnchanged(t *testing.T) {
	c0, c1, c2 := chunk(1), chunk(2), chunk(3)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1, c2}, nil)) // B's OLD content
	newSize := int64(3 * ChunkSize)
	// A's NEW hashes: chunk 1 differs (a chunk of 9s), 0 and 2 match B's cache.
	hashes := bytes.Join([][]byte{h16(c0), h16(chunk(9)), h16(c2)}, nil)

	nb := runDiff(t, bitmapHaving(3, 0, 1, 2), path, &mockHashProvider{size: newSize, hashes: hashes}, newSize)
	waitFor(t, 3*time.Second, func() bool { return nb.Has(0) && nb.Has(2) })
	if !nb.Has(0) || !nb.Has(2) {
		t.Fatalf("unchanged chunks 0,2 must be kept (0=%v 2=%v)", nb.Has(0), nb.Has(2))
	}
	if nb.Has(1) {
		t.Fatal("changed chunk 1 must NOT be kept (must re-fetch)")
	}
}

// Shrink: file goes 3 chunks -> 2; the surviving identical prefix is kept.
func TestEditDiff_ShrinkKeepsSurvivingPrefix(t *testing.T) {
	c0, c1, c2 := chunk(1), chunk(2), chunk(3)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1, c2}, nil))
	newSize := int64(2 * ChunkSize) // dropped the last chunk
	hashes := bytes.Join([][]byte{h16(c0), h16(c1)}, nil)

	nb := runDiff(t, bitmapHaving(3, 0, 1, 2), path, &mockHashProvider{size: newSize, hashes: hashes}, newSize)
	waitFor(t, 3*time.Second, func() bool { return nb.Has(0) && nb.Has(1) })
	if !nb.Has(0) || !nb.Has(1) {
		t.Fatalf("surviving chunks 0,1 must be kept (0=%v 1=%v)", nb.Has(0), nb.Has(1))
	}
}

// Grow: file goes 2 chunks -> 3; old identical chunks kept, the new chunk is
// missing (B never had it) and gets fetched.
func TestEditDiff_GrowKeepsOldMissesNew(t *testing.T) {
	c0, c1 := chunk(1), chunk(2)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1}, nil)) // B only ever had 2 chunks
	newSize := int64(3 * ChunkSize)
	hashes := bytes.Join([][]byte{h16(c0), h16(c1), h16(chunk(7))}, nil) // chunk 2 is new

	nb := runDiff(t, bitmapHaving(2, 0, 1), path, &mockHashProvider{size: newSize, hashes: hashes}, newSize)
	waitFor(t, 3*time.Second, func() bool { return nb.Has(0) && nb.Has(1) })
	if !nb.Has(0) || !nb.Has(1) {
		t.Fatalf("old chunks 0,1 must be kept (0=%v 1=%v)", nb.Has(0), nb.Has(1))
	}
	if nb.Has(2) {
		t.Fatal("new chunk 2 must be missing (B never had it)")
	}
}

// A chunk B never cached is skipped even if its (would-be) hash matches — B has no
// bytes to keep, so it must be fetched.
func TestEditDiff_SkipsChunksWeNeverHad(t *testing.T) {
	c0, c1, c2 := chunk(1), chunk(2), chunk(3)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1, c2}, nil))
	newSize := int64(3 * ChunkSize)
	hashes := bytes.Join([][]byte{h16(c0), h16(c1), h16(c2)}, nil) // all match the cache

	// B only had chunk 0; 1 and 2 were never cached.
	nb := runDiff(t, bitmapHaving(3, 0), path, &mockHashProvider{size: newSize, hashes: hashes}, newSize)
	waitFor(t, 3*time.Second, func() bool { return nb.Has(0) })
	time.Sleep(150 * time.Millisecond)
	if !nb.Has(0) {
		t.Fatal("cached + matching chunk 0 must be kept")
	}
	if nb.Has(1) || nb.Has(2) {
		t.Fatal("chunks never cached (1,2) must stay missing even though hashes match")
	}
}

// Old peer / RPC error -> full re-fetch (bitmap stays all-missing).
func TestEditDiff_FallbackOnError(t *testing.T) {
	c0, c1 := chunk(1), chunk(2)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1}, nil))
	newSize := int64(2 * ChunkSize)

	nb := runDiff(t, bitmapHaving(2, 0, 1), path, &mockHashProvider{err: errors.New("unimplemented")}, newSize)
	time.Sleep(200 * time.Millisecond)
	if nb.Have() != 0 {
		t.Fatalf("on RPC error the bitmap must stay all-missing (full re-fetch), got Have=%d", nb.Have())
	}
}

// A size that no longer matches what the edit recorded -> don't trust the hashes.
func TestEditDiff_RejectsSizeMismatch(t *testing.T) {
	c0, c1 := chunk(1), chunk(2)
	path := writeCacheFile(t, bytes.Join([][]byte{c0, c1}, nil))
	newSize := int64(2 * ChunkSize)
	// Peer reports a different size than the edit recorded (it moved again).
	hashes := bytes.Join([][]byte{h16(c0), h16(c1)}, nil)

	nb := runDiff(t, bitmapHaving(2, 0, 1), path, &mockHashProvider{size: newSize + int64(ChunkSize), hashes: hashes}, newSize)
	time.Sleep(200 * time.Millisecond)
	if nb.Have() != 0 {
		t.Fatalf("on size mismatch the bitmap must stay all-missing, got Have=%d", nb.Have())
	}
}
