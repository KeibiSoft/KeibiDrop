// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// A read outside a proven stream fetches one ProbeFetch unit, and twenty such
// probes across a file (an NLE's filmstrip) move at most twenty units, never
// the whole file. Measured before this change: all five blocks of a cold 82 MB
// clip, 100%, for twenty single-frame probes.
func TestProbeFetch_ColdProbesFetchUnitsNotBlocks(t *testing.T) {
	const blocks = 5
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	probe := func(off int64) {
		got := make([]byte, 4096)
		n := root.Read("/f.bin", got, off, fh)
		if n != len(got) {
			t.Fatalf("probe at %d read %d bytes, want %d", off, n, len(got))
		}
		if !bytes.Equal(got, content[off:off+int64(n)]) {
			t.Fatalf("probe at %d: content mismatch", off)
		}
	}

	probe(40 * 1024 * 1024)
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	if r, b := prov.reads.Load(), prov.bytesIn.Load(); r != 1 || b != int64(ProbeFetch) {
		t.Fatalf("one cold probe: fetches=%d bytes=%d, want one fetch of %d", r, b, ProbeFetch)
	}

	for i := 0; i < 20; i++ {
		probe(int64(fileSize) * int64(i) / 20)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	if b := prov.bytesIn.Load(); b > 21*int64(ProbeFetch) {
		t.Fatalf("filmstrip of 20 probes moved %d bytes, want at most %d", b, 21*ProbeFetch)
	}
	if c := root.raPrefetchCalls.Load(); c != 0 {
		t.Fatalf("scattered probes prefetched (%d calls), want 0", c)
	}
}

// A sequential reader fetches units on demand until the window arms, then
// the window fetches whole blocks ahead of it. No byte is fetched twice.
func TestProbeFetch_SequentialStreamFetchesEveryByteOnce(t *testing.T) {
	const blocks = 5
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	step := 128 * 1024
	buf := make([]byte, step)
	read := func(off int) {
		n := root.Read("/f.bin", buf, int64(off), fh)
		if n != step {
			t.Fatalf("read at %d returned %d bytes, want %d", off, n, step)
		}
		if !bytes.Equal(buf, content[off:off+step]) {
			t.Fatalf("read at %d: content mismatch", off)
		}
	}

	// Below the gate: one fetch per unit, nothing prefetched.
	gate := ReadAheadBlock / 2
	for off := 0; off+step < gate; off += step {
		read(off)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	wantUnits := int64(gate / ProbeFetch)
	if r, b := prov.reads.Load(), prov.bytesIn.Load(); r != wantUnits || b != int64(gate) {
		t.Fatalf("below the gate: fetches=%d bytes=%d, want %d units of %d", r, b, wantUnits, ProbeFetch)
	}
	if c := root.raPrefetchCalls.Load(); c != 0 {
		t.Fatalf("prefetched below the gate (%d calls)", c)
	}

	// Through the gate: the rest of block 0 still comes in units on demand and
	// the window fetches the blocks behind it, with no byte fetched twice.
	for off := gate - step; off+step <= 2*ReadAheadBlock; off += step {
		read(off)
	}
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })
	if b := prov.bytesIn.Load(); b != int64(fileSize) {
		t.Fatalf("bytes fetched %d, want %d: every byte once, block 0 in units plus the window", b, fileSize)
	}
	if r := prov.reads.Load(); r != int64(ReadAheadBlock/ProbeFetch)+4 {
		t.Fatalf("fetches=%d, want %d: block 0 in units, four window blocks", r, ReadAheadBlock/ProbeFetch+4)
	}
}

// gateProvider serves like countingProvider, but holds the fetch that starts at
// gateAt until open closes, so a test can act while that fetch is in flight.
type gateProvider struct {
	countingProvider
	gateAt int64
	open   chan struct{}
}

func (p *gateProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &gateStream{countingStream: countingStream{p: &p.countingProvider}, g: p}, nil
}

type gateStream struct {
	countingStream
	g *gateProvider
}

func (s *gateStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	if offset == s.g.gateAt {
		select {
		case <-s.g.open:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.countingStream.ReadAt(ctx, offset, size)
}

// The window fetches its blocks one after another. While it is busy with one,
// a parallel read (a FUSE worker, the kernel's look-ahead) lands the first unit
// of the next. The window then starts that block after the landed unit. It
// fetched the block from its start, and those 2 MiB moved twice.
func TestProbeFetch_WindowSkipsAUnitAReadLanded(t *testing.T) {
	const blocks = 4
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	prov := &gateProvider{countingProvider: countingProvider{content: content}, gateAt: int64(ReadAheadBlock), open: make(chan struct{})}
	root, fh, cleanup := newReadAheadFileWith(t, int64(fileSize), prov)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	buf := make([]byte, 128*1024)
	read := func(off int) {
		if n := root.Read("/f.bin", buf, int64(off), fh); n != len(buf) || !bytes.Equal(buf, content[off:off+len(buf)]) {
			t.Fatalf("read at %d returned %d bytes or wrong bytes", off, n)
		}
	}

	// A stream proves itself in block 0. The window takes blocks 1 to 4 and
	// holds on block 1.
	for off := 0; off <= ReadAheadBlock/2; off += len(buf) {
		read(off)
	}
	waitFor(t, 3*time.Second, func() bool { return root.raPrefetchCalls.Load() == 1 })

	// A read lands the first unit of block 2 while the window holds on block 1.
	read(2 * ReadAheadBlock)
	waitFor(t, 3*time.Second, func() bool { return f.Bitmap.HasRange(int64(2*ReadAheadBlock), ProbeFetch) })

	close(prov.open)
	got := readSeq(root, fh, fileSize, 128)
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch")
	}
	if b := prov.bytesIn.Load(); b != int64(fileSize) {
		t.Fatalf("bytes fetched %d, want %d: the window fetched the unit the read landed again", b, fileSize)
	}
}

// FUSE hands a sequential scan to several worker threads, so reads arrive in
// parallel and out of order, and the kernel's look-ahead runs ahead of the
// application. Whatever the order, every byte is fetched once: a read that
// starts inside a fetch in flight joins it.
func TestProbeFetch_ParallelOutOfOrderReadersFetchEveryByteOnce(t *testing.T) {
	const blocks = 3
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	const workers = 4
	step := 1024 * 1024 // the kernel's read size
	half := step / 2    // and its alignment
	var offsets []int
	for off := 0; off+step <= fileSize; off += half {
		offsets = append(offsets, off)
	}
	var wg sync.WaitGroup
	var bad atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			buf := make([]byte, step)
			for i := w; i < len(offsets); i += workers {
				off := offsets[i]
				if n := root.Read("/f.bin", buf, int64(off), fh); n != step || !bytes.Equal(buf, content[off:off+step]) {
					bad.Add(1)
				}
			}
		}(w)
	}
	wg.Wait()
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })
	if bad.Load() != 0 {
		t.Fatalf("%d reads returned wrong bytes", bad.Load())
	}
	if b := prov.bytesIn.Load(); b != int64(fileSize) {
		t.Fatalf("bytes fetched %d, want %d: every byte once even with parallel out-of-order reads", b, fileSize)
	}
}

// A read that crosses a unit boundary (the kernel reads 1 to 2 MiB at a time)
// fetches the whole span of units it touches, once, and the read that follows
// in the second unit is served from cache.
func TestProbeFetch_StraddlingReadFetchesTheSpanOnce(t *testing.T) {
	fileSize := 3 * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	read := func(off, n int) {
		got := make([]byte, n)
		if got_n := root.Read("/f.bin", got, int64(off), fh); got_n != n {
			t.Fatalf("read at %d returned %d bytes, want %d", off, got_n, n)
		}
		if !bytes.Equal(got, content[off:off+n]) {
			t.Fatalf("read at %d: content mismatch", off)
		}
	}
	mib := 1024 * 1024
	read(ProbeFetch-mib/2, mib) // straddles the end of unit 0
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	if r, b := prov.reads.Load(), prov.bytesIn.Load(); r != 1 || b != 2*int64(ProbeFetch) {
		t.Fatalf("straddling read: fetches=%d bytes=%d, want one fetch of two units (%d)", r, b, 2*ProbeFetch)
	}
	read(ProbeFetch+mib/2, mib) // inside unit 1, which the span landed
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	if r := prov.reads.Load(); r != 1 {
		t.Fatalf("the read after the straddle fetched again (%d fetches)", r)
	}
}
