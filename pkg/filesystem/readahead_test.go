// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file tests on-demand read-ahead: one 16 MiB fetch per block, singleflight
// coalescing of concurrent readers, and byte integrity across block boundaries.

package filesystem

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// countingProvider serves a content buffer and counts on-demand fetches so a
// test can assert "one fetch per read-ahead block", not one per 512 KiB chunk.
type countingProvider struct {
	content []byte
	reads   atomic.Int64 // ReadAt calls (on-demand fetches).
	bytesIn atomic.Int64 // Total bytes requested across fetches.
}

func (p *countingProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &countingStream{p: p}, nil
}

func (p *countingProvider) StreamFile(_ context.Context, _ string, startOffset uint64) (types.StreamFileReceiver, error) {
	return &slowStreamReceiver{content: p.content, offset: startOffset}, nil
}

type countingStream struct{ p *countingProvider }

func (s *countingStream) ReadAt(_ context.Context, offset int64, size int64) ([]byte, error) {
	s.p.reads.Add(1)
	s.p.bytesIn.Add(size)
	content := s.p.content
	if offset > int64(len(content)) {
		offset = int64(len(content))
	}
	end := offset + size
	if end > int64(len(content)) {
		end = int64(len(content))
	}
	out := make([]byte, end-offset)
	copy(out, content[offset:end])
	return out, nil
}

func (s *countingStream) Close() error { return nil }

// makePattern returns n deterministic, never-zero bytes, so any zero byte read
// back signals a sparse-hole (the corruption class this feature must not cause).
func makePattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251) + 1
	}
	return b
}

// newReadAheadFile wires a root Dir with one on-demand File backed by a
// pre-allocated sparse cache file and the counting provider, mirroring the
// production on-demand setup. Declared size equals the content length.
func newReadAheadFile(t *testing.T, content []byte) (*Dir, uint64, *countingProvider, func()) {
	t.Helper()
	prov := &countingProvider{content: content}
	root, fh, cleanup := newReadAheadFileWith(t, int64(len(content)), prov)
	return root, fh, prov, cleanup
}

// newReadAheadFileWith wires a root Dir with one on-demand File of the given
// declared size, served by prov. declaredSize may exceed the provider's actual
// content to simulate a remote file shorter than its cached stat (truncation race).
func newReadAheadFileWith(t *testing.T, declaredSize int64, prov types.FileStreamProvider) (*Dir, uint64, func()) {
	t.Helper()
	saveDir := t.TempDir()

	cacheFD, err := os.OpenFile(filepath.Join(saveDir, "cache.bin"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatalf("create cache file: %v", err)
	}
	if err := cacheFD.Truncate(declaredSize); err != nil {
		t.Fatalf("truncate cache file: %v", err)
	}

	root := &Dir{
		RelativePath:        "/",
		LocalDownloadFolder: saveDir,
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
		FsCtx:               context.Background(),
	}
	root.Root = root
	root.logger = nopLogger()

	f := &File{
		logger:         nopLogger(),
		Root:           root,
		Parent:         root,
		NotLocalSynced: true,
		Bitmap:         NewChunkBitmap(declaredSize),
		CacheFD:        cacheFD,
		StreamProvider: prov,
		stat:           &winfuse.Stat_t{Size: declaredSize},
	}

	fh := allocHandleID()
	root.OpenFileHandlers[fh] = &HandleEntry{FD: int(cacheFD.Fd()), File: f}

	return root, fh, func() { _ = cacheFD.Close() }
}

// readSeq reads the whole file sequentially in stepKiB-sized reads and returns
// the assembled bytes.
func readSeq(root *Dir, fh uint64, fileSize, stepKiB int) []byte {
	got := make([]byte, 0, fileSize)
	buf := make([]byte, stepKiB*1024)
	for off := int64(0); off < int64(fileSize); {
		n := root.Read("/f.bin", buf, off, fh)
		if n <= 0 {
			break
		}
		got = append(got, buf[:n]...)
		off += int64(n)
	}
	return got
}

// A sequential pass over a multi-block file must fetch each 16 MiB block exactly
// once (not once per 512 KiB chunk), and return byte-correct content.
func TestReadAhead_OneFetchPerBlock(t *testing.T) {
	fileSize := 2 * ReadAheadBlock // exactly two read-ahead blocks
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()

	got := readSeq(root, fh, fileSize, 128)
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(content))
	}

	wantFetches := int64(fileSize / ReadAheadBlock) // 2
	if got := prov.reads.Load(); got != wantFetches {
		t.Fatalf("fetches = %d, want %d (one per %d-byte block, not per chunk)",
			got, wantFetches, ReadAheadBlock)
	}
}

// Concurrent reads of the same block must collapse onto a single fetch.
func TestReadAhead_ParallelReadsCoalesce(t *testing.T) {
	fileSize := ReadAheadBlock // one block
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()

	const readers = 16
	var wg sync.WaitGroup
	bad := make([]bool, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 64*1024)
			off := int64(i) * 64 * 1024 // distinct offsets, all inside block 0
			n := root.Read("/f.bin", buf, off, fh)
			if n != len(buf) || !bytes.Equal(buf[:n], content[off:off+int64(n)]) {
				bad[i] = true
			}
		}(i)
	}
	wg.Wait()

	if got := prov.reads.Load(); got != 1 {
		t.Fatalf("parallel readers issued %d fetches, want 1 (singleflight)", got)
	}
	for i, b := range bad {
		if b {
			t.Fatalf("reader %d read wrong bytes", i)
		}
	}
}

// Random scattered reads across block boundaries must always return the exact
// content (never a sparse-hole zero from the pre-allocated cache file).
func TestReadAhead_RandomSeeksNoSparseZeros(t *testing.T) {
	fileSize := 3*ReadAheadBlock + 123*1024
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()

	ra := int64(ReadAheadBlock)
	offsets := []int64{
		0, 4096, ra - 1024, ra, ra + 1024, 2*ra - 4096, 2 * ra,
		2*ra + 512*1024, 3 * ra, int64(fileSize) - 4096, int64(fileSize) - 1,
	}
	buf := make([]byte, 64*1024)
	for _, off := range offsets {
		want := content[off:]
		if len(want) > len(buf) {
			want = want[:len(buf)]
		}
		n := root.Read("/f.bin", buf[:len(want)], off, fh)
		if n != len(want) || !bytes.Equal(buf[:n], want) {
			t.Fatalf("read at %d: got %d bytes, want %d (sparse hole or short read)",
				off, n, len(want))
		}
	}
	// Read-ahead means a handful of block fetches, not one per 512 KiB chunk.
	if reads := prov.reads.Load(); reads > 16 {
		t.Fatalf("random seeks issued %d fetches; read-ahead should keep this small", reads)
	}
}

// A coalesced waiter must not serve sparse-hole zeros when the leader's block
// comes back short because the remote file is shorter than the cached size
// (truncation / EOF race). This reproduces the exact reviewer-found bug.
func TestReadAhead_ShortBlockWaiterNoSparseZeros(t *testing.T) {
	declared := int64(ReadAheadBlock)          // File believes block 0 is 16 MiB...
	content := makePattern(ReadAheadBlock / 2) // ...but the remote only has 8 MiB.
	prov := &slowStreamProvider{content: content, delay: 30 * time.Millisecond}
	root, fh, cleanup := newReadAheadFileWith(t, declared, prov)
	defer cleanup()

	tailOff := int64(len(content)) + 512*1024 // inside block 0 but past real content
	const readers = 8
	var wg sync.WaitGroup
	var sparse, headWrong atomic.Int64
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 64*1024)
			if i == 0 {
				n := root.Read("/f.bin", buf, 0, fh)
				if n != len(buf) || !bytes.Equal(buf[:n], content[:n]) {
					headWrong.Add(1)
				}
				return
			}
			// These coalesce on block 0 with the slow leader, then read a region
			// the remote never had. They must get EOF (0), not sparse zeros.
			if n := root.Read("/f.bin", buf, tailOff, fh); n != 0 {
				sparse.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if sparse.Load() != 0 {
		t.Fatalf("%d coalesced waiters served sparse-hole bytes past the real content", sparse.Load())
	}
	if headWrong.Load() != 0 {
		t.Fatalf("head reader got wrong or short bytes")
	}
}

// A read that straddles a 16 MiB block boundary must return ALL requested bytes
// (a mid-file short read can be misread as EOF by the kernel).
func TestReadAhead_StraddleBlockBoundary(t *testing.T) {
	fileSize := 2 * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, _, cleanup := newReadAheadFile(t, content)
	defer cleanup()

	off := int64(ReadAheadBlock) - 100*1024 // starts before the boundary
	buf := make([]byte, 200*1024)           // ends after it
	n := root.Read("/f.bin", buf, off, fh)
	if n != len(buf) {
		t.Fatalf("straddling read returned %d, want %d (short read mid-file)", n, len(buf))
	}
	if !bytes.Equal(buf, content[off:off+int64(len(buf))]) {
		t.Fatalf("straddling read returned corrupt bytes")
	}
}

// The final partial block (file size not a multiple of 16 MiB) reads correctly,
// and a read at EOF returns 0.
func TestReadAhead_PartialLastBlockEOF(t *testing.T) {
	fileSize := ReadAheadBlock + 300*1024
	content := makePattern(fileSize)
	root, fh, _, cleanup := newReadAheadFile(t, content)
	defer cleanup()

	off := int64(ReadAheadBlock)
	buf := make([]byte, 512*1024) // larger than the 300 KiB that remains
	n := root.Read("/f.bin", buf, off, fh)
	if int64(n) != 300*1024 {
		t.Fatalf("partial last block read %d bytes, want %d", n, 300*1024)
	}
	if !bytes.Equal(buf[:n], content[off:]) {
		t.Fatalf("partial last block corrupt")
	}

	if n := root.Read("/f.bin", buf, int64(fileSize), fh); n != 0 {
		t.Fatalf("read at EOF returned %d, want 0", n)
	}
}

// Opening a folder makes Windows Explorer / macOS Photos fire Open+tiny-read+Close
// on many files at once to build previews. The thumbnail gate must keep each file to
// the block(s) it actually probes and NEVER trigger the read-ahead window — otherwise
// a folder scan pulls every file and saturates the link. This is the 25+-files-at-once
// case: each thumbnailer reads a small header + trailer (the moov-atom pattern) far below
// the half-block gate, so no file may fetch more than its two probed blocks.
func TestReadAhead_ThumbnailStormNoWholeFilePrefetch(t *testing.T) {
	const nFiles = 25
	fileSize := 6 * ReadAheadBlock // 6 blocks: a 4-block prefetch window would be unmistakable
	probe := 256 * 1024            // 256 KiB probe, far below the half-block (8 MiB) consume gate
	content := makePattern(fileSize)
	buf := make([]byte, probe)

	for i := 0; i < nFiles; i++ {
		root, fh, prov, cleanup := newReadAheadFile(t, content)
		// thumbnailer: header read, then a trailer read, then "close".
		if n := root.Read("/f.bin", buf, 0, fh); n <= 0 {
			t.Fatalf("file %d header read: %d", i, n)
		}
		if n := root.Read("/f.bin", buf, int64(fileSize-probe), fh); n <= 0 {
			t.Fatalf("file %d trailer read: %d", i, n)
		}
		// Each probe pulls only its own 16 MiB block on the miss; the gate must add
		// NOTHING. At most two blocks (header + trailer), never the 4-block window.
		if got := prov.reads.Load(); got > 2 {
			t.Fatalf("file %d: thumbnail probe fetched %d blocks -> read-ahead window fired (gate failed); want <=2 (header+trailer only)", i, got)
		}
		// And it must never have pulled the whole file.
		if got := prov.bytesIn.Load(); got > int64(3*ReadAheadBlock) {
			t.Fatalf("file %d: thumbnail pulled %d bytes (~%d blocks) of a %d-block file -- whole-file prefetch", i, got, got/int64(ReadAheadBlock), fileSize/ReadAheadBlock)
		}
		cleanup()
	}
}
