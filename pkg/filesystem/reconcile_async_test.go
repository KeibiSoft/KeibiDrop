// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: tests the async partial-invalidation that runs after a remote-edit reset.
// ABOUTME: kept chunks become present on the live bitmap; changed/grown stay absent.

package filesystem

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// hashReceiver replays a fixed set of (chunkIndex, hash) pairs then io.EOF.
type hashReceiver struct {
	pairs []hashPair
	i     int
}

type hashPair struct {
	idx  uint64
	hash uint64
}

func (r *hashReceiver) Recv() (uint64, uint64, error) {
	if r.i >= len(r.pairs) {
		return 0, 0, io.EOF
	}
	p := r.pairs[r.i]
	r.i++
	return p.idx, p.hash, nil
}

// hashingProvider is a FileStreamProvider that ALSO implements types.ChunkHasher.
// GetChunkHashes returns peerHashes[c] for chunks in [fromChunk, fromChunk+count).
// If err is set, GetChunkHashes returns it instead (fallback path).
type hashingProvider struct {
	peerHashes map[int]uint64
	err        error
}

func (p *hashingProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &errorStream{err: io.EOF}, nil
}
func (p *hashingProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, io.EOF
}
func (p *hashingProvider) GetChunkHashes(_ context.Context, _ string, _, fromChunk, count uint64) (types.ChunkHashReceiver, error) {
	if p.err != nil {
		return nil, p.err
	}
	pairs := make([]hashPair, 0, count)
	for c := fromChunk; c < fromChunk+count; c++ {
		if h, ok := p.peerHashes[int(c)]; ok {
			pairs = append(pairs, hashPair{idx: c, hash: h})
		}
	}
	return &hashReceiver{pairs: pairs}, nil
}

// plainProvider implements only FileStreamProvider, NOT ChunkHasher (older peer).
type plainProvider struct{}

func (p *plainProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &errorStream{err: io.EOF}, nil
}
func (p *plainProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, io.EOF
}

// makeOldBitmap returns a hashed bitmap of the given size where the named chunks
// are present with the given hashes (the "what we have cached" state pre-edit).
func makeOldBitmap(fileSize int64, chunkHashes map[int]uint64) *ChunkBitmap {
	b := NewChunkBitmap(fileSize)
	for c, h := range chunkHashes {
		b.Set(c)
		b.SetHash(c, h)
	}
	return b
}

// newReconcileDir builds a minimal Dir wired with prov for reconcileEditAsync.
func newReconcileDir(prov types.FileStreamProvider) *Dir {
	d := &Dir{
		RelativePath:       "/",
		OpenFileHandlers:   make(map[uint64]*HandleEntry),
		AllDirMap:          make(map[string]*Dir),
		AllFileMap:         make(map[string]*File),
		RemoteFiles:        make(map[string]*File),
		FsCtx:              context.Background(),
		PrefetchSem:        make(chan struct{}, 8),
		OpenStreamProvider: func() types.FileStreamProvider { return prov },
	}
	d.Root = d
	d.logger = nopLogger()
	return d
}

// reconcileSize is a comfortably-above-threshold file size with whole chunks.
// reconcileMinSize is 16 MiB; use 40 chunks (20 MiB) so the gate passes.
const reconcileChunks = 40

func reconcileSize() int64 { return int64(reconcileChunks) * int64(ChunkSize) }

// TestReconcileEditAsync_KeepsMatchingDropsChanged drives reconcileEditAsync with a
// matching peer for most chunks and a changed hash for one. After the async job, the
// matching chunks must be present (cache hit) on the LIVE bitmap and the changed one
// must stay absent (re-fetch).
func TestReconcileEditAsync_KeepsMatchingDropsChanged(t *testing.T) {
	size := reconcileSize()
	// Old: chunks 0..4 present+hashed.
	old := makeOldBitmap(size, map[int]uint64{0: 0xA, 1: 0xB, 2: 0xC, 3: 0xD, 4: 0xE})
	// Peer agrees on 0,1,3,4; chunk 2 changed.
	prov := &hashingProvider{peerHashes: map[int]uint64{0: 0xA, 1: 0xB, 2: 0xFF, 3: 0xD, 4: 0xE}}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	// Live bitmap = the fully-reset bitmap reads now use.
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.reconcileEditAsync("/edited.bin", f, old, newBitmap, size, size)

	// Await the async job by polling for chunk 0 becoming present.
	waitFor(t, 3*time.Second, func() bool { return newBitmap.Has(0) })

	for _, c := range []int{0, 1, 3, 4} {
		if !newBitmap.Has(c) {
			t.Fatalf("chunk %d (unchanged) should be present on the live bitmap", c)
		}
		h, ok := newBitmap.Hash(c)
		if !ok {
			t.Fatalf("chunk %d should carry a hash", c)
		}
		want := map[int]uint64{0: 0xA, 1: 0xB, 3: 0xD, 4: 0xE}[c]
		if h != want {
			t.Fatalf("chunk %d hash = %#x, want %#x", c, h, want)
		}
	}
	if newBitmap.Has(2) {
		t.Fatal("chunk 2 (changed) must stay absent so it is re-fetched")
	}
}

// TestReconcileEditAsync_BoundaryAndGrownStayAbsent uses a non-chunk-aligned oldSize
// and a grown newSize. The boundary chunk straddling min(old,new) and grown-region
// chunks must never be marked present, even when the peer reports a matching hash.
func TestReconcileEditAsync_BoundaryAndGrownStayAbsent(t *testing.T) {
	cs := int64(ChunkSize)
	// old has 22 whole chunks + 100 bytes (chunk 22 partial). new grows to 40 chunks.
	oldSize := 22*cs + 100
	newSize := reconcileSize()
	hashes := map[int]uint64{}
	for c := 0; c <= 22; c++ {
		hashes[c] = uint64(0x100 + c)
	}
	old := makeOldBitmap(oldSize, hashes)
	// Peer reports a matching hash for the boundary chunk 22 and grown chunk 30.
	peer := map[int]uint64{}
	for c := 0; c <= 22; c++ {
		peer[c] = uint64(0x100 + c)
	}
	peer[30] = 0x999
	prov := &hashingProvider{peerHashes: peer}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: newSize}}
	newBitmap := NewChunkBitmap(newSize)
	f.Bitmap = newBitmap

	d.reconcileEditAsync("/grown.bin", f, old, newBitmap, oldSize, newSize)

	// Whole common-prefix chunks 0..21 must become present.
	waitFor(t, 3*time.Second, func() bool { return newBitmap.Has(21) })

	for c := 0; c <= 21; c++ {
		if !newBitmap.Has(c) {
			t.Fatalf("whole common-prefix chunk %d should be present", c)
		}
	}
	if newBitmap.Has(22) {
		t.Fatal("boundary chunk 22 (straddles min(old,new)) must stay absent")
	}
	if newBitmap.Has(30) {
		t.Fatal("grown-region chunk 30 must stay absent")
	}
}

// TestReconcileEditAsync_FallbackNotChunkHasher: a provider that is not a ChunkHasher
// leaves the live bitmap fully reset (today's behavior, full re-fetch).
func TestReconcileEditAsync_FallbackNotChunkHasher(t *testing.T) {
	size := reconcileSize()
	old := makeOldBitmap(size, map[int]uint64{0: 0xA, 1: 0xB})
	d := newReconcileDir(&plainProvider{}) // no GetChunkHashes

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.reconcileEditAsync("/x.bin", f, old, newBitmap, size, size)

	// Give any (incorrectly spawned) job time to run, then assert nothing kept.
	time.Sleep(100 * time.Millisecond)
	if newBitmap.Have() != 0 {
		t.Fatalf("non-ChunkHasher peer must leave a full reset, got Have=%d", newBitmap.Have())
	}
}

// TestReconcileEditAsync_NilProvider_NoPanicFullReset: when OpenStreamProvider
// returns a nil provider (the Shutdown race: kd.session nil'd before FsCtx is
// cancelled, so the closure now returns nil instead of dereferencing it), the
// ChunkHasher type assertion fails gracefully (ok == false) and the async job is
// a no-op. No panic, and the full reset stands (live bitmap untouched).
func TestReconcileEditAsync_NilProvider_NoPanicFullReset(t *testing.T) {
	size := reconcileSize()
	old := makeOldBitmap(size, map[int]uint64{0: 0xA, 1: 0xB})
	d := newReconcileDir(nil) // OpenStreamProvider returns a nil interface

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.reconcileEditAsync("/x.bin", f, old, newBitmap, size, size)

	time.Sleep(100 * time.Millisecond)
	if newBitmap.Have() != 0 {
		t.Fatalf("nil provider must leave a full reset, got Have=%d", newBitmap.Have())
	}
}

// TestReconcileEditAsync_FallbackUnimplemented: a ChunkHasher whose GetChunkHashes
// returns codes.Unimplemented leaves the live bitmap fully reset.
func TestReconcileEditAsync_FallbackUnimplemented(t *testing.T) {
	size := reconcileSize()
	old := makeOldBitmap(size, map[int]uint64{0: 0xA, 1: 0xB})
	prov := &hashingProvider{err: status.Error(codes.Unimplemented, "older peer")}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.reconcileEditAsync("/x.bin", f, old, newBitmap, size, size)

	time.Sleep(100 * time.Millisecond)
	if newBitmap.Have() != 0 {
		t.Fatalf("Unimplemented peer must leave a full reset, got Have=%d", newBitmap.Have())
	}
}

// TestMaybeReconcileEdit_GateBelowMinSizeSkips: a tiny file (< reconcileMinSize) must
// not reconcile even if everything else is eligible (cheaper to just re-pull).
func TestMaybeReconcileEdit_GateBelowMinSizeSkips(t *testing.T) {
	size := int64(ChunkSize) // 512 KiB, well below the 16 MiB threshold
	old := makeOldBitmap(size, map[int]uint64{0: 0xA})
	prov := &hashingProvider{peerHashes: map[int]uint64{0: 0xA}}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.maybeReconcileEdit("/tiny.bin", f, old, newBitmap, size, size)

	time.Sleep(100 * time.Millisecond)
	if newBitmap.Have() != 0 {
		t.Fatalf("below-min-size file must skip reconcile, got Have=%d", newBitmap.Have())
	}
}

// TestMaybeReconcileEdit_GateNoOldHashesSkips: an old bitmap with no fingerprints
// (fresh / disk-loaded) must skip reconcile (nothing to compare against).
func TestMaybeReconcileEdit_GateNoOldHashesSkips(t *testing.T) {
	size := reconcileSize()
	old := NewChunkBitmap(size)
	old.Set(0) // present bit, but no SetHash => HasHashes() is false
	prov := &hashingProvider{peerHashes: map[int]uint64{0: 0xA}}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	d.maybeReconcileEdit("/nohashes.bin", f, old, newBitmap, size, size)

	time.Sleep(100 * time.Millisecond)
	if newBitmap.Have() != 0 {
		t.Fatalf("old bitmap without hashes must skip reconcile, got Have=%d", newBitmap.Have())
	}
}

// TestReconcileEditAsync_RaceWithConcurrentReader runs the async job while a second
// goroutine hammers the SAME live bitmap with Has/Set/SetHash, mimicking an on-demand
// Read that fetched new content for a chunk and marked it present concurrently. Run
// under -race, this exercises the shared-bitmap mutation the job introduces. No race
// must be reported, and the unchanged chunk the job keeps must end up present.
func TestReconcileEditAsync_RaceWithConcurrentReader(t *testing.T) {
	size := reconcileSize()
	hashes := map[int]uint64{}
	peer := map[int]uint64{}
	for c := 0; c < reconcileChunks; c++ {
		hashes[c] = uint64(0x500 + c)
		peer[c] = uint64(0x500 + c) // peer agrees on everything: all kept
	}
	old := makeOldBitmap(size, hashes)
	prov := &hashingProvider{peerHashes: peer}
	d := newReconcileDir(prov)

	f := &File{logger: nopLogger(), Root: d, stat: &winfuse.Stat_t{Size: size}}
	newBitmap := NewChunkBitmap(size)
	f.Bitmap = newBitmap

	var wg sync.WaitGroup
	wg.Add(1)
	// Concurrent "reader": fetches chunk 7 (sets it present with a fresh hash) and
	// reads the bitmap throughout, racing the async job's Set/SetHash on other chunks.
	go func() {
		defer wg.Done()
		for i := 0; i < 5000; i++ {
			_ = newBitmap.Has(i % reconcileChunks)
			if i == 0 {
				newBitmap.Set(7)
				newBitmap.SetHash(7, 0x507)
			}
		}
	}()

	d.reconcileEditAsync("/race.bin", f, old, newBitmap, size, size)
	wg.Wait()

	waitFor(t, 3*time.Second, func() bool { return newBitmap.Has(reconcileChunks - 1) })
	for c := 0; c < reconcileChunks; c++ {
		if !newBitmap.Has(c) {
			t.Fatalf("chunk %d (all peer-matched) should be present after reconcile", c)
		}
	}
}

// TestEditRemoteFile_ReconcilesInPlace drives the FULL EditRemoteFile handler: a file
// already in RemoteFiles with a hashed bitmap gets edited (newer mtime, same size). The
// handler does its full reset, then the async job re-marks the unchanged chunks present.
func TestEditRemoteFile_ReconcilesInPlace(t *testing.T) {
	size := reconcileSize()
	prov := &hashingProvider{peerHashes: map[int]uint64{0: 0xA, 1: 0xB, 2: 0xFF, 3: 0xD}}
	d := newReconcileDir(prov)

	// Seed an existing remote file whose cached bitmap is hashed.
	old := makeOldBitmap(size, map[int]uint64{0: 0xA, 1: 0xB, 2: 0xC, 3: 0xD})
	seed := &winfuse.Stat_t{Size: size, Mode: 0o644 | winfuse.S_IFREG}
	seed.Mtim.Sec = 10
	f := &File{
		logger:       nopLogger(),
		Root:         d,
		stat:         seed,
		RelativePath: "/edited.bin",
		Name:         "edited.bin",
		Bitmap:       old,
	}
	d.RemoteFiles["/edited.bin"] = f
	d.AllFileMap["/edited.bin"] = f

	// Edit: same size, NEWER mtime => in-place edit reset path.
	edit := &winfuse.Stat_t{Size: size, Mode: 0o644 | winfuse.S_IFREG}
	edit.Mtim.Sec = 20
	if err := d.EditRemoteFile(nopLogger(), "/edited.bin", "edited.bin", edit); err != nil {
		t.Fatalf("EditRemoteFile: %v", err)
	}

	// The handler swapped f.Bitmap to a fresh reset bitmap. Grab the live one.
	d.RemoteFilesLock.RLock()
	live := f.Bitmap
	d.RemoteFilesLock.RUnlock()
	if live == old {
		t.Fatal("EditRemoteFile must replace the bitmap with a fresh reset (full re-fetch)")
	}

	waitFor(t, 3*time.Second, func() bool { return live.Has(0) })
	for _, c := range []int{0, 1, 3} {
		if !live.Has(c) {
			t.Fatalf("chunk %d (unchanged) should be re-marked present after edit", c)
		}
	}
	if live.Has(2) {
		t.Fatal("chunk 2 (changed) must stay absent for re-fetch")
	}
}
