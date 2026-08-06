// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// Tests for predictive sequential read-ahead (fetch the next block(s) before the
// read head arrives, self-tuning by hit/miss feedback, reset on seek) and for the
// on-demand read failure classification (NotFound -> EOF, stall -> EIO).

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// waitFor polls cond until it is true or the timeout elapses; it fails the test
// on timeout. Used to await asynchronous prefetch goroutines deterministically.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", timeout)
	}
}

// errorStream / errorProvider serve every ReadAt with a fixed error, to exercise
// the on-demand fetch-failure classification in Read.
type errorStream struct{ err error }

func (s *errorStream) ReadAt(_ context.Context, _ int64, _ int64) ([]byte, error) {
	return nil, s.err
}
func (s *errorStream) Close() error { return nil }

type errorProvider struct{ err error }

func (p *errorProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &errorStream{err: p.err}, nil
}
func (p *errorProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, p.err
}

// With read-ahead enabled, reading only block 0 (past its midpoint) must fetch
// block 1 ahead of time, even though the test never reads block 1.
func TestReadAhead_PrefetchesNextBlockAhead(t *testing.T) {
	fileSize := 3 * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4 // enable read-ahead (64 MiB cap)
	f := root.OpenFileHandlers[fh].File

	// Read all of block 0 (crossing its midpoint) in small steps. Never read block 1.
	buf := make([]byte, 128*1024)
	for off := int64(0); off < int64(ReadAheadBlock); off += int64(len(buf)) {
		if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
			t.Fatalf("read at %d returned %d", off, n)
		}
	}

	// Predictive read-ahead must bring block 1 into cache without it being read.
	waitFor(t, 3*time.Second, func() bool {
		return f.Bitmap.HasRange(int64(ReadAheadBlock), ChunkSize)
	})

	// Wait for every in-flight fetch to settle, then confirm block 1 is present
	// and that reading it now is served locally and byte-correct.
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
	got := make([]byte, 128*1024)
	n := root.Read("/f.bin", got, int64(ReadAheadBlock), fh)
	if n != len(got) {
		t.Fatalf("read of prefetched block 1 returned %d, want %d", n, len(got))
	}
	want := content[ReadAheadBlock : ReadAheadBlock+len(got)]
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("prefetched block 1 byte %d mismatch", i)
		}
	}
	_ = prov
}

// Read-ahead must fire SPARSELY: a steady sequential pass in tiny reads issues a
// prefetch at most about once per 16 MiB block the head crosses, NOT once per
// 512 KiB/64 KiB read. Re-issuing per small read is the bug this guards against.
func TestReadAhead_SparseTrigger(t *testing.T) {
	const blocks = 6
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	const stepKiB = 64
	got := readSeq(root, fh, fileSize, stepKiB)
	if len(got) != fileSize {
		t.Fatalf("short read: %d of %d", len(got), fileSize)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	numReads := int64(fileSize / (stepKiB * 1024)) // = blocks*256, the per-read count
	calls := root.raPrefetchCalls.Load()
	t.Logf("read-ahead issued %d prefetch calls for %d blocks over %d reads", calls, blocks, numReads)

	// Sparse: bounded by blocks (+ the initial window burst), nowhere near per-read.
	if maxCalls := int64(2 * (blocks + root.ReadAheadWindowBlocks)); calls > maxCalls {
		t.Fatalf("read-ahead not sparse: %d prefetch calls (> %d) for %d blocks", calls, maxCalls, blocks)
	}
	// Hard guard on the regression: must be an order of magnitude below the reads.
	if calls*10 >= numReads {
		t.Fatalf("read-ahead fires per-read: %d calls vs %d reads (must be <<)", calls, numReads)
	}
	// And still correct: each block fetched exactly once.
	if prov.reads.Load() != int64(blocks) {
		t.Fatalf("fetches=%d want %d", prov.reads.Load(), blocks)
	}
}

// A whole-file sequential pass with read-ahead enabled must still fetch each
// block exactly once: prefetch and on-demand reads coalesce via singleflight, so
// no block is fetched twice.
func TestReadAhead_NoDoubleFetchWithPrefetch(t *testing.T) {
	const blocks = 4
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File

	got := readSeq(root, fh, fileSize, 128)
	if len(got) != fileSize {
		t.Fatalf("read %d bytes, want %d", len(got), fileSize)
	}
	for i := range got {
		if got[i] != content[i] {
			t.Fatalf("content mismatch at %d", i)
		}
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	if reads := prov.reads.Load(); reads != int64(blocks) {
		t.Fatalf("fetches = %d, want %d (one per block; prefetch must not duplicate)", reads, blocks)
	}
}

// With read-ahead disabled (window 0), reading block 0 must NOT prefetch block 1:
// behavior is identical to pure on-demand. This guards the off switch.
func TestReadAhead_DisabledDoesNotPrefetch(t *testing.T) {
	fileSize := 3 * ReadAheadBlock
	content := makePattern(fileSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	root.ReadAheadWindowBlocks = 0 // disabled
	f := root.OpenFileHandlers[fh].File

	buf := make([]byte, 128*1024)
	for off := int64(0); off < int64(ReadAheadBlock); off += int64(len(buf)) {
		if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
			t.Fatalf("read at %d returned %d", off, n)
		}
	}
	waitFor(t, 2*time.Second, func() bool { return inflightEmpty(f) })

	if reads := prov.reads.Load(); reads != 1 {
		t.Fatalf("disabled read-ahead fetched %d blocks, want 1 (block 0 only)", reads)
	}
	if f.Bitmap.HasRange(int64(ReadAheadBlock), ChunkSize) {
		t.Fatalf("disabled read-ahead prefetched block 1")
	}
}

// raState reads maybeReadAhead's detector state under its lock.
func raState(f *File) (window int, frontier int64) {
	f.raMu.Lock()
	defer f.raMu.Unlock()
	return f.raWindowBlocks, f.raPrefetchedTo
}

// raPool builds a stream pool for driving maybeReadAhead directly.
func raPool(t *testing.T, prov *countingProvider, fh uint64) *StreamPool {
	t.Helper()
	p, err := NewStreamPool(prov, context.Background(), fh, "/f.bin", StreamPoolSize)
	if err != nil {
		t.Fatalf("new stream pool: %v", err)
	}
	return p
}

// White-box: on a confirmed sequential run the read-ahead primes STRAIGHT to the
// full window (no slow ramp) so smooth playback is reached fast; reads before a
// block midpoint or inside the already-buffered region issue nothing (sparse);
// and a refill tops the buffer back up once it drains past half the window.
func TestReadAhead_PrimesFullWindowOnSequential(t *testing.T) {
	const capW = 4
	fileSize := 16 * ReadAheadBlock
	root, fh, prov, cleanup := newReadAheadFile(t, makePattern(fileSize))
	defer cleanup()
	root.ReadAheadWindowBlocks = capW
	f := root.OpenFileHandlers[fh].File
	ra := int64(ReadAheadBlock)
	chunk := int64(ChunkSize)
	pool := raPool(t, prov, fh)
	defer func() { _ = pool.Close() }()
	// stream consumes [from,to) in realistic on-demand-sized chunks, like a player
	// reading sequentially — the detector keys off bytes CONSUMED, not a single call.
	stream := func(from, to int64) {
		for o := from; o < to; o += chunk {
			root.maybeReadAhead(f, pool, f.CacheFD, f.Bitmap, o, int(chunk), int64(fileSize))
		}
	}

	// Consuming less than half a block must NOT commit to read-ahead (frontier put).
	stream(0, ra/2-chunk)
	if _, fr := raState(f); fr > 1 {
		t.Fatalf("early (<half block) reads advanced the frontier to %d", fr)
	}
	if c := root.raPrefetchCalls.Load(); c != 0 {
		t.Fatalf("early (<half block) reads prefetched (%d calls)", c)
	}
	// Crossing half a block of real consumption -> prime straight to the full window.
	stream(ra/2-chunk, ra/2+chunk)
	if w, fr := raState(f); w != capW || fr != 1+int64(capW) {
		t.Fatalf("after half a block consumed: window=%d frontier=%d, want %d,%d", w, fr, capW, 1+capW)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) }) // let the sequential prefetch finish

	// Reads still inside the buffered region (> half the window ahead) are sparse:
	// the frontier does not advance, so no new prefetch is issued.
	_, frBefore := raState(f)
	stream(ra/2+chunk, ra+chunk)
	if _, fr := raState(f); fr != frBefore {
		t.Fatalf("reads inside the buffer advanced the frontier %d -> %d (not sparse)", frBefore, fr)
	}
	// Draining past half the window -> a refill tops back up to a full window.
	stream(ra+chunk, 2*ra+chunk)
	if w, fr := raState(f); w != capW || fr != 2+1+int64(capW) {
		t.Fatalf("after drain refill: window=%d frontier=%d, want %d,%d", w, fr, capW, 2+1+capW)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })
}

// A non-sequential jump resets the window to 1 and restarts the frontier just
// past the new position, and prefetches NOTHING on the jump read itself (so a
// mispredicted random access wastes no bandwidth).
func TestReadAhead_JumpResetsAndPrefetchesNothing(t *testing.T) {
	fileSize := 64 * ReadAheadBlock
	root, fh, prov, cleanup := newReadAheadFile(t, makePattern(fileSize))
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File
	ra := int64(ReadAheadBlock)
	chunk := int64(ChunkSize)
	pool := raPool(t, prov, fh)
	defer func() { _ = pool.Close() }()
	rd := func(off int64) { root.maybeReadAhead(f, pool, f.CacheFD, f.Bitmap, off, 64*1024, int64(fileSize)) }

	// A sequential run that consumes enough commits to read-ahead and primes the window.
	for o := int64(0); o < ra/2+chunk; o += chunk {
		root.maybeReadAhead(f, pool, f.CacheFD, f.Bitmap, o, int(chunk), int64(fileSize))
	}
	if w, _ := raState(f); w < 2 {
		t.Fatalf("window did not ramp on a sequential run: %d", w)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	// A big jump far past the frontier: reset, and no prefetch on the jump read.
	before := root.raPrefetchCalls.Load()
	const jumpBlock = 40
	rd(jumpBlock * ra)
	if w, fr := raState(f); w != 1 || fr != jumpBlock+1 {
		t.Fatalf("after jump: window=%d frontier=%d, want 1,%d", w, fr, jumpBlock+1)
	}
	if root.raPrefetchCalls.Load() != before {
		t.Fatalf("jump read issued prefetch; it must prefetch nothing")
	}
}

// A lone small read at the start (a thumbnailer probing a header) must not
// trigger any read-ahead.
func TestReadAhead_HeaderProbeNoPrefetch(t *testing.T) {
	fileSize := 8 * ReadAheadBlock
	root, fh, prov, cleanup := newReadAheadFile(t, makePattern(fileSize))
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File
	pool := raPool(t, prov, fh)
	defer func() { _ = pool.Close() }()

	root.maybeReadAhead(f, pool, f.CacheFD, f.Bitmap, 0, 64*1024, int64(fileSize))
	if c := root.raPrefetchCalls.Load(); c != 0 {
		t.Fatalf("header probe prefetched (%d calls), want 0", c)
	}
}

// A transient fetch failure (not NotFound) after retries must surface as EIO, not
// a false EOF that a media player would read as end-of-file mid-stream.
func TestReadAhead_StallReturnsEIO(t *testing.T) {
	declared := int64(ReadAheadBlock)
	prov := &errorProvider{err: errors.New("transient network stall")}
	root, fh, cleanup := newReadAheadFileWith(t, declared, prov)
	defer cleanup()

	buf := make([]byte, 64*1024)
	n := root.Read("/f.bin", buf, 0, fh)
	if n != -winfuse.EIO {
		t.Fatalf("stalled read returned %d, want -EIO (%d)", n, -winfuse.EIO)
	}
}

// When the peer reports the file is gone (NotFound), the read returns EOF (0),
// preserving the long-standing behavior for transient files (e.g. git .keep).
func TestReadAhead_NotFoundReturnsEOF(t *testing.T) {
	declared := int64(ReadAheadBlock)
	prov := &errorProvider{err: types.ErrRemoteFileNotFound}
	root, fh, cleanup := newReadAheadFileWith(t, declared, prov)
	defer cleanup()

	buf := make([]byte, 64*1024)
	n := root.Read("/f.bin", buf, 0, fh)
	if n != 0 {
		t.Fatalf("read of a not-found file returned %d, want 0 (EOF)", n)
	}
}

// inflightEmpty reports whether read-ahead is idle: no single-flight block fetch
// is outstanding AND the sequential prefetch goroutine has finished. Tests wait on
// this before asserting, so async prefetch cannot race the assertions.
func inflightEmpty(f *File) bool {
	f.fetchMu.Lock()
	n := len(f.inflight)
	f.fetchMu.Unlock()
	f.raMu.Lock()
	prefetching := f.raPrefetching
	f.raMu.Unlock()
	return n == 0 && !prefetching
}

// delayProvider serves content but adds a fixed latency to every fetch, modeling
// a high-RTT link so a test can show read-ahead hides that latency.
type delayProvider struct {
	content []byte
	delay   time.Duration
	reads   atomic.Int64
}

func (p *delayProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &delayStream{p: p}, nil
}
func (p *delayProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, errors.New("unused")
}

type delayStream struct{ p *delayProvider }

func (s *delayStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	select {
	case <-time.After(s.p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.p.reads.Add(1)
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
func (s *delayStream) Close() error { return nil }

// genProvider serves a large synthetic file WITHOUT a full in-memory buffer
// (bytes generated by offset), so a test can model a 600 MB media file cheaply.
// It counts on-demand fetches and bytes, and counts (and refuses) any whole-file
// StreamFile prefetch, which is the behavior the thumbnail scenario must avoid.
// An optional delay models link latency.
type genProvider struct {
	size            int64
	delay           time.Duration
	reads           atomic.Int64
	bytesIn         atomic.Int64
	streamFileCalls atomic.Int64
}

func (p *genProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &genStream{p: p}, nil
}
func (p *genProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	p.streamFileCalls.Add(1)
	return nil, errors.New("whole-file prefetch must not run during a thumbnail scan")
}

type genStream struct{ p *genProvider }

func (s *genStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	if s.p.delay > 0 {
		select {
		case <-time.After(s.p.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.p.reads.Add(1)
	if offset >= s.p.size {
		return []byte{}, nil
	}
	if offset+size > s.p.size {
		size = s.p.size - offset
	}
	s.p.bytesIn.Add(size)
	out := make([]byte, size)
	for i := range out {
		out[i] = byte((offset+int64(i))%251) + 1
	}
	return out, nil
}
func (s *genStream) Close() error { return nil }

// newThumbnailDir builds one root holding on-demand files of the given (mixed)
// sizes with sparse caches, modeling a folder of variable-size media shared
// on-demand. Works with *testing.T and *testing.B.
func newThumbnailDir(tb testing.TB, sizes []int64, delay time.Duration) (*Dir, []uint64, []*genProvider, func()) {
	tb.Helper()
	saveDir := tb.TempDir()
	root := &Dir{
		RelativePath:        "/",
		LocalDownloadFolder: saveDir,
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
	}
	root.Root = root
	root.logger = nopLogger()
	root.SetCtx(context.Background())
	root.ReadAheadWindowBlocks = readAheadWindowBlocks(64) // default read-ahead on

	var fhs []uint64
	var provs []*genProvider
	var fds []*os.File
	for i, size := range sizes {
		cacheFD, err := os.OpenFile(filepath.Join(saveDir, fmt.Sprintf("f%d.bin", i)), os.O_RDWR|os.O_CREATE, 0600)
		if err != nil {
			tb.Fatalf("create cache file: %v", err)
		}
		if err := cacheFD.Truncate(size); err != nil { // sparse: no real disk until written
			tb.Fatalf("truncate cache file: %v", err)
		}
		prov := &genProvider{size: size, delay: delay}
		f := &File{
			logger:         nopLogger(),
			Root:           root,
			Parent:         root,
			NotLocalSynced: true,
			Bitmap:         NewChunkBitmap(size),
			CacheFD:        cacheFD,
			StreamProvider: prov,
			stat:           &winfuse.Stat_t{Size: size},
		}
		fh := allocHandleID()
		root.OpenFileHandlers[fh] = &HandleEntry{FD: int(cacheFD.Fd()), File: f}
		fhs = append(fhs, fh)
		provs = append(provs, prov)
		fds = append(fds, cacheFD)
	}
	return root, fhs, provs, func() {
		for _, fd := range fds {
			_ = fd.Close()
		}
	}
}

// thumbnailSpots returns the offsets a media thumbnailer touches for a file of
// the given size: the header, plus (for larger files) an interior frame and the
// tail (some container formats keep the index/moov atom near the end). All small
// reads, never a sequential pass. Offsets are valid in-file positions.
func thumbnailSpots(size int64) []int64 {
	if size <= 0 {
		return nil
	}
	spots := []int64{0} // header
	if size > int64(ReadAheadBlock) {
		spots = append(spots, size/2) // an interior frame, in a different block
	}
	if size > 128*1024 {
		spots = append(spots, size-64*1024) // index / moov atom near the end
	}
	return spots
}

// The Windows scenario: Explorer + Photos open a folder of variable-size media
// shared on-demand and read scattered small ranges to make thumbnails. This must
// NOT turn into a whole-file download per file (the freeze the user hit). With the
// default config (prefetch off) and bounded read-ahead, each file fetches only the
// few blocks it touches, no StreamFile prefetch runs, and the scan finishes fast.
func TestThumbnailScan_OnDemandStaysSmall(t *testing.T) {
	const MB = 1024 * 1024
	// A realistic mixed folder of 25 variable-size media files shared on-demand:
	// big videos, medium clips, large/RAW photos, photos, and small photos. The
	// caches are sparse, so the large declared sizes cost no disk until a block is
	// actually fetched.
	var sizes []int64
	for k := 0; k < 5; k++ {
		sizes = append(sizes, 600*MB) // big videos
	}
	for k := 0; k < 6; k++ {
		sizes = append(sizes, 100*MB) // medium clips
	}
	for k := 0; k < 6; k++ {
		sizes = append(sizes, 20*MB) // short clips / RAW photos
	}
	for k := 0; k < 5; k++ {
		sizes = append(sizes, 2*MB) // photos
	}
	for k := 0; k < 3; k++ {
		sizes = append(sizes, 500*1024) // small photos
	}
	// 25 files total.
	root, fhs, provs, cleanup := newThumbnailDir(t, sizes, 0)
	defer cleanup()

	// Open and probe every file at once, as Explorer + Photos do for a folder.
	var wg sync.WaitGroup
	for i := range sizes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buf := make([]byte, 64*1024)
			for _, off := range thumbnailSpots(sizes[i]) {
				if n := root.Read("/f.bin", buf, off, fhs[i]); n <= 0 {
					t.Errorf("file %d (size %d) thumbnail read at %d returned %d", i, sizes[i], off, n)
					return
				}
			}
		}(i)
	}
	// A whole-file download of any big file would hang this; the whole concurrent
	// scan must finish promptly.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent thumbnail scan hung — opening a file likely triggered a whole-file download")
	}

	for i, prov := range provs {
		size := sizes[i]
		f := root.OpenFileHandlers[fhs[i]].File
		// 15s, not 5s: Windows/WinFsp drains the async cache writes more slowly, and a
		// too-tight wait flaked here ("condition not met within 5s"). The assertion below
		// (no whole-file prefetch) is what matters, not how fast in-flight ops settle.
		waitFor(t, 15*time.Second, func() bool { return inflightEmpty(f) })

		if c := prov.streamFileCalls.Load(); c != 0 {
			t.Fatalf("file %d (size %d): whole-file prefetch ran %d times during a thumbnail scan", i, size, c)
		}
		fetched := prov.bytesIn.Load()
		// Fetch is bounded by the few blocks the thumbnailer touched, regardless of
		// file size (random access must not grow the read-ahead window).
		spots := int64(len(thumbnailSpots(size)))
		if maxBytes := (spots + 1) * int64(ReadAheadBlock); fetched > maxBytes {
			t.Fatalf("file %d (size %d): over-fetched %d bytes (> %d) — random access not bounded", i, size, fetched, maxBytes)
		}
		// For genuinely large (multi-block) files, a thumbnail must read far less
		// than the whole file — that is the freeze this fix prevents.
		if size > int64(4)*int64(ReadAheadBlock) && fetched >= size {
			t.Fatalf("file %d (size %d): thumbnail downloaded the whole file (%d bytes)", i, size, fetched)
		}
	}
}

// BenchmarkThumbnailScan measures a cold thumbnail scan of a large on-demand file
// over a simulated high-latency link. The scan time must scale with the few ranges
// touched (a handful of fetches), NOT with the file size — proving opening a 640 MB
// file to make a thumbnail does not download it.
func BenchmarkThumbnailScan(b *testing.B) {
	fileSize := int64(40) * int64(ReadAheadBlock) // 640 MiB, sparse
	delay := 5 * time.Millisecond                 // simulated per-fetch RTT
	buf := make([]byte, 64*1024)
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		root, fhs, provs, cleanup := newThumbnailDir(b, []int64{fileSize}, delay)
		b.StartTimer()
		for _, off := range thumbnailSpots(fileSize) {
			if got := root.Read("/f.bin", buf, off, fhs[0]); got <= 0 {
				b.Fatalf("thumbnail read at %d returned %d", off, got)
			}
		}
		b.StopTimer()
		if provs[0].streamFileCalls.Load() != 0 {
			b.Fatalf("whole-file prefetch ran during thumbnail benchmark")
		}
		cleanup()
		b.StartTimer()
	}
}

// Under injected per-fetch latency and a PACED consumer (a real player at a fixed
// bitrate, the case that matters — a memory-speed greedy reader outruns any
// sequential prefetch and is not a real workload), read-ahead must keep the buffer
// full so block boundaries stop stalling. This is the in-process analogue of the
// WAN benchmark; the real run toggles KEIBIDROP_READ_AHEAD_WINDOW_MB 0 vs 64.
func TestReadAhead_LatencyHidingSpeedup(t *testing.T) {
	const blocks = 8
	const chunk = int64(512 * 1024)
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)
	delay := 30 * time.Millisecond
	// Pace chosen for CI robustness under -race: a block plays for ~400ms (16 MiB /
	// 40 MB/s) >> the 30ms fetch, so a working prefetch stays well ahead even on a
	// slow/contended runner; the per-chunk budget (~13ms) is below the 30ms fetch, so
	// an OFF cold read reliably overruns it (a stall) while cached reads do not.
	const paceMBps = 40.0

	run := func(window int) (stalls int, reads int64) {
		prov := &delayProvider{content: content, delay: delay}
		root, fh, cleanup := newReadAheadFileWith(t, int64(fileSize), prov)
		defer cleanup()
		root.ReadAheadWindowBlocks = window
		f := root.OpenFileHandlers[fh].File
		budget := time.Duration(float64(chunk) / (paceMBps * 1e6) * float64(time.Second))
		buf := make([]byte, chunk)
		start := time.Now()
		for off := int64(0); off < int64(fileSize); off += chunk {
			target := start.Add(time.Duration(off/chunk) * budget)
			if dwell := time.Until(target); dwell > 0 {
				time.Sleep(dwell)
			}
			tc := time.Now()
			if n := root.Read("/f.bin", buf, off, fh); n <= 0 {
				t.Fatalf("read at %d returned %d", off, n)
			}
			if time.Since(tc) > budget { // the read blocked past the play budget: a stall the viewer feels
				stalls++
			}
		}
		waitFor(t, 10*time.Second, func() bool { return inflightEmpty(f) })
		return stalls, prov.reads.Load()
	}

	offStalls, offReads := run(0)
	onStalls, onReads := run(4)
	t.Logf("paced %gMB/s, fetch delay %v: OFF stalls=%d (%d fetches); ON(window=4) stalls=%d (%d fetches)",
		paceMBps, delay, offStalls, offReads, onStalls, onReads)

	if onReads != int64(blocks) || offReads != int64(blocks) {
		t.Fatalf("expected exactly %d fetches each (no duplicates): off=%d on=%d", blocks, offReads, onReads)
	}
	// Timing-robust assertions. CI under -race is noisy and inflates BOTH stall counts
	// (a slow cached read can miss the play budget, and an OFF cold stall cascades into
	// the next deadlines). So we do NOT assert an absolute ON ceiling; instead require
	// OFF to actually stall at boundaries, and read-ahead to ELIMINATE at least half the
	// block-boundary stalls. The difference cancels symmetric per-read noise.
	if offStalls < blocks/2 {
		t.Fatalf("OFF did not stall at block boundaries (got %d); test not exercising the cold path", offStalls)
	}
	if offStalls-onStalls < blocks/2 {
		t.Fatalf("read-ahead did not hide block-boundary stalls: ON=%d OFF=%d (want at least %d fewer)", onStalls, offStalls, blocks/2)
	}
}

// FUSE delivers a single sequential scan as PARALLEL, out-of-order reads (one per
// worker thread). Read-ahead must detect the stream regardless of arrival order.
// This is the regression test for the bug that made read-ahead a no-op on WinFsp:
// a "is this read right after the previous one?" stride test saw every out-of-order
// read as a backward seek and never prefetched. The high-water-mark detector keys
// off distance from the read head and total bytes consumed, so order does not matter.
func TestReadAhead_ParallelOutOfOrderReadsStillPrefetch(t *testing.T) {
	fileSize := 8 * ReadAheadBlock
	root, fh, prov, cleanup := newReadAheadFile(t, makePattern(fileSize))
	defer cleanup()
	root.ReadAheadWindowBlocks = 4
	f := root.OpenFileHandlers[fh].File
	pool := raPool(t, prov, fh)
	defer func() { _ = pool.Close() }()
	ra := int64(ReadAheadBlock)
	cs := int64(128 * 1024)

	// Walk the first 1.5 blocks but swap adjacent offsets so they arrive
	// NON-monotonically — the pattern a stride detector misreads as constant seeks,
	// while the scan still advances overall.
	var offs []int64
	for o := int64(0); o < ra+ra/2; o += cs {
		offs = append(offs, o)
	}
	for i := 0; i+1 < len(offs); i += 2 {
		offs[i], offs[i+1] = offs[i+1], offs[i]
	}
	for _, o := range offs {
		root.maybeReadAhead(f, pool, f.CacheFD, f.Bitmap, o, int(cs), int64(fileSize))
	}
	waitFor(t, 5*time.Second, func() bool { return inflightEmpty(f) })

	if _, fr := raState(f); fr <= 1 {
		t.Fatalf("out-of-order (parallel) reads did not trigger read-ahead: frontier=%d (the WinFsp no-op bug)", fr)
	}
	if root.raPrefetchCalls.Load() == 0 {
		t.Fatalf("no blocks prefetched under parallel/out-of-order reads")
	}
}
