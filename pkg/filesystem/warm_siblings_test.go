// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Unit tests for the sibling warmer: claims, coalescing, failures,
// ABOUTME: bitmap swaps, exclusions, sticky unsupported, metadata apply.

package filesystem

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// warmBatchProvider is a FileStreamProvider + BatchReader fake. ReadBatch
// serves one frame per path from files, like the real server for files under
// the frame cap. release, when set, blocks frame delivery until closed.
type warmBatchProvider struct {
	mu         sync.Mutex
	files      map[string][]byte
	lastPaths  []string
	perFileErr map[string]uint32
	batchErr   error
	started    chan struct{}
	release    chan struct{}

	batchCalls  atomic.Int64
	readAtCalls atomic.Int64
}

func (p *warmBatchProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &warmCountStream{p: p}, nil
}

func (p *warmBatchProvider) StreamFile(_ context.Context, _ string, _ uint64) (types.StreamFileReceiver, error) {
	return nil, errors.New("not used in warm tests")
}

func (p *warmBatchProvider) ReadBatch(_ context.Context, paths []string, maxBytes uint64) (types.BatchFrameReceiver, error) {
	p.batchCalls.Add(1)
	p.mu.Lock()
	p.lastPaths = append([]string(nil), paths...)
	p.mu.Unlock()
	if p.started != nil {
		select {
		case p.started <- struct{}{}:
		default:
		}
	}
	if p.batchErr != nil {
		return nil, p.batchErr
	}
	var frames []types.BatchFrame
	var sent uint64
	for _, path := range paths {
		if sent >= maxBytes {
			break
		}
		if code, bad := p.perFileErr[path]; bad {
			frames = append(frames, types.BatchFrame{Path: path, ErrCode: code, FileDone: true})
			continue
		}
		data, ok := p.files[path]
		if !ok {
			frames = append(frames, types.BatchFrame{Path: path, ErrCode: 1, FileDone: true})
			continue
		}
		frames = append(frames, types.BatchFrame{Path: path, Data: data, TotalSize: uint64(len(data)), FileDone: true})
		sent += uint64(len(data))
	}
	return &fakeBatchReceiver{p: p, frames: frames}, nil
}

type fakeBatchReceiver struct {
	p      *warmBatchProvider
	frames []types.BatchFrame
	idx    int
}

func (r *fakeBatchReceiver) Recv() (types.BatchFrame, error) {
	if r.p.release != nil {
		select {
		case <-r.p.release:
		case <-time.After(10 * time.Second):
			return types.BatchFrame{}, errors.New("test gate timeout")
		}
	}
	if r.idx >= len(r.frames) {
		return types.BatchFrame{}, io.EOF
	}
	f := r.frames[r.idx]
	r.idx++
	return f, nil
}

type warmCountStream struct{ p *warmBatchProvider }

func (s *warmCountStream) ReadAt(_ context.Context, _ int64, _ int64) ([]byte, error) {
	s.p.readAtCalls.Add(1)
	return nil, errors.New("per-file fetch not expected in this test")
}
func (s *warmCountStream) Close() error { return nil }

// newWarmRoot builds a root Dir with the fake provider and announces the
// given files with the given stat template.
func newWarmRoot(t *testing.T, provider *warmBatchProvider, statFor func(size int64) *winfuse.Stat_t) *Dir {
	t.Helper()
	saveDir := t.TempDir()
	root := &Dir{
		RelativePath:        "/",
		RealPathOfFile:      saveDir,
		LocalDownloadFolder: saveDir,
		IsLocalPresent:      true,
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
		AllDirMap:           make(map[string]*Dir),
		AllFileMap:          make(map[string]*File),
		RemoteFiles:         make(map[string]*File),
		PrefetchSem:         make(chan struct{}, 8),
		warmDirs:            make(map[string]time.Time),
	}
	root.Root = root
	root.logger = nopLogger()
	root.SetCtx(context.Background())
	root.SetStreamProvider(func() types.FileStreamProvider { return provider })

	for path, data := range provider.files {
		if err := root.AddRemoteFile(root.logger, path, filepath.Base(path), statFor(int64(len(data)))); err != nil {
			t.Fatalf("AddRemoteFile(%s): %v", path, err)
		}
	}
	return root
}

func plainStat(size int64) *winfuse.Stat_t {
	return &winfuse.Stat_t{Size: size, Mode: 0o644 | winfuse.S_IFREG}
}

func remoteFile(t *testing.T, root *Dir, path string) *File {
	t.Helper()
	root.RemoteFilesLock.RLock()
	f := root.RemoteFiles[path]
	root.RemoteFilesLock.RUnlock()
	if f == nil {
		t.Fatalf("remote file %s missing", path)
	}
	return f
}

func warmSettled(root *Dir, dirPath string) func() bool {
	return func() bool {
		root.warmMu.Lock()
		next, exists := root.warmDirs[dirPath]
		root.warmMu.Unlock()
		return !exists || !next.IsZero()
	}
}

// TestWarmSiblings_WarmsDirectory: one trigger warms every announced small
// sibling: bytes on disk, chunks marked with hashes, download counters full,
// NotLocalSynced cleared, and the trigger file itself left alone.
func TestWarmSiblings_WarmsDirectory(t *testing.T) {
	files := map[string][]byte{}
	names := []string{"/evd/a.bin", "/evd/b.bin", "/evd/c.bin", "/evd/d.bin", "/evd/e.bin"}
	for i, n := range names {
		files[n] = makePattern(10*1024 + i*512)
	}
	p := &warmBatchProvider{files: files}
	root := newWarmRoot(t, p, plainStat)

	root.maybeWarmSiblings("/evd/b.bin")
	waitFor(t, 5*time.Second, warmSettled(root, "/evd"))

	p.mu.Lock()
	requested := append([]string(nil), p.lastPaths...)
	p.mu.Unlock()
	for _, rp := range requested {
		if rp == "/evd/b.bin" {
			t.Fatal("trigger file must not be in the batch")
		}
	}
	if len(requested) != len(names)-1 {
		t.Fatalf("requested %d paths, want %d", len(requested), len(names)-1)
	}
	// Extraction frontier: the first requested sibling is the one after the trigger.
	if requested[0] != "/evd/c.bin" {
		t.Fatalf("first requested = %s, want /evd/c.bin (rotation after trigger)", requested[0])
	}

	for _, n := range names {
		f := remoteFile(t, root, n)
		if n == "/evd/b.bin" {
			if f.Bitmap.IsComplete() {
				t.Fatal("trigger file must stay unwarmed")
			}
			continue
		}
		if !f.Bitmap.IsComplete() {
			t.Fatalf("%s bitmap incomplete after warm", n)
		}
		if _, ok := f.Bitmap.Hash(0); !ok {
			t.Fatalf("%s chunk 0 has no hash", n)
		}
		f.metaMu.RLock()
		nls := f.NotLocalSynced
		f.metaMu.RUnlock()
		if nls {
			t.Fatalf("%s still NotLocalSynced", n)
		}
		if got := f.Download.BytesDownloaded.Load(); got != uint64(len(files[n])) {
			t.Fatalf("%s download counter %d, want %d", n, got, len(files[n]))
		}
		disk, err := os.ReadFile(filepath.Join(root.LocalDownloadFolder, n))
		if err != nil {
			t.Fatalf("%s not on disk: %v", n, err)
		}
		if string(disk) != string(files[n]) {
			t.Fatalf("%s bytes differ on disk", n)
		}
	}
	if got := root.warmFiles.Load(); got != int64(len(names)-1) {
		t.Fatalf("warmFiles = %d, want %d", got, len(names)-1)
	}
	if got := root.warmBatches.Load(); got != 1 {
		t.Fatalf("warmBatches = %d, want 1", got)
	}
	if got := p.readAtCalls.Load(); got != 0 {
		t.Fatalf("per-file fetches = %d, want 0", got)
	}
}

// TestWarmSiblings_ConcurrentReaderCoalesces: a reader arriving while the warm
// holds the claim waits on it and then serves from cache, with zero extra
// network fetches.
func TestWarmSiblings_ConcurrentReaderCoalesces(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
	}
	p := &warmBatchProvider{
		files:   files,
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	root := newWarmRoot(t, p, plainStat)
	f := remoteFile(t, root, "/evd/b.bin")

	root.maybeWarmSiblings("/evd/a.bin")
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never started")
	}

	// The warm owns b's claim now: a reader must NOT become leader.
	bf, leader := f.beginBlockFetch(0)
	if leader {
		t.Fatal("reader became leader while warm held the claim")
	}
	close(p.release)

	settled, err := bf.wait(context.Background())
	if !settled || err != nil {
		t.Fatalf("wait: settled=%v err=%v", settled, err)
	}
	if !f.Bitmap.HasRange(0, len(files["/evd/b.bin"])) {
		t.Fatal("cache not complete after coalesced wait")
	}
	if got := p.readAtCalls.Load(); got != 0 {
		t.Fatalf("per-file fetches = %d, want 0 (no double-fetch)", got)
	}
}

// TestWarmSiblings_BatchErrorReleasesClaims: a failed batch releases every
// claim, leaves the files fetchable, and puts the directory in cooldown.
func TestWarmSiblings_BatchErrorReleasesClaims(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
	}
	p := &warmBatchProvider{files: files, batchErr: errors.New("wire down")}
	root := newWarmRoot(t, p, plainStat)
	f := remoteFile(t, root, "/evd/b.bin")

	root.maybeWarmSiblings("/evd/a.bin")
	waitFor(t, 5*time.Second, warmSettled(root, "/evd"))

	bf, leader := f.beginBlockFetch(0)
	if !leader {
		t.Fatal("claim not released after batch failure")
	}
	f.finishBlockFetch(0, bf, nil)
	if f.Bitmap.IsComplete() {
		t.Fatal("file must stay unwarmed after batch failure")
	}

	// Cooldown: an immediate re-trigger does not start a second batch.
	root.maybeWarmSiblings("/evd/a.bin")
	time.Sleep(100 * time.Millisecond)
	if got := p.batchCalls.Load(); got != 1 {
		t.Fatalf("batchCalls = %d, want 1 (cooldown must hold)", got)
	}
}

// TestWarmSiblings_BitmapSwapMidWarm: an announce that swaps the bitmap while
// the batch is in flight voids the claim: no marks land in the new bitmap.
func TestWarmSiblings_BitmapSwapMidWarm(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
	}
	p := &warmBatchProvider{
		files:   files,
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	root := newWarmRoot(t, p, plainStat)
	f := remoteFile(t, root, "/evd/b.bin")

	root.maybeWarmSiblings("/evd/a.bin")
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never started")
	}

	// Simulate an edit announce landing mid-warm: fresh bitmap.
	f.metaMu.Lock()
	f.Bitmap = NewChunkBitmap(int64(len(files["/evd/b.bin"])))
	fresh := f.Bitmap
	f.metaMu.Unlock()

	close(p.release)
	waitFor(t, 5*time.Second, warmSettled(root, "/evd"))

	if fresh.Has(0) {
		t.Fatal("stale warm bytes marked in the swapped bitmap")
	}
	f.metaMu.RLock()
	nls := f.NotLocalSynced
	f.metaMu.RUnlock()
	if !nls {
		t.Fatal("NotLocalSynced must survive a voided warm")
	}
}

// TestWarmSiblings_EditedFilesExcluded: LocalNewer or HadEdits keeps a file
// out of the batch entirely.
func TestWarmSiblings_EditedFilesExcluded(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
		"/evd/c.bin": makePattern(7 * 1024),
	}
	p := &warmBatchProvider{files: files}
	root := newWarmRoot(t, p, plainStat)
	fb := remoteFile(t, root, "/evd/b.bin")
	fb.metaMu.Lock()
	fb.LocalNewer = true
	fb.metaMu.Unlock()
	fc := remoteFile(t, root, "/evd/c.bin")
	fc.metaMu.Lock()
	fc.HadEdits = true
	fc.metaMu.Unlock()

	root.maybeWarmSiblings("/evd/a.bin")
	waitFor(t, 5*time.Second, warmSettled(root, "/evd"))

	p.mu.Lock()
	requested := append([]string(nil), p.lastPaths...)
	p.mu.Unlock()
	for _, rp := range requested {
		if rp == "/evd/b.bin" || rp == "/evd/c.bin" {
			t.Fatalf("edited file %s must not be warmed", rp)
		}
	}
	if fb.Bitmap.IsComplete() || fc.Bitmap.IsComplete() {
		t.Fatal("edited files must stay untouched")
	}
}

// TestWarmSiblings_UnsupportedSticky: a peer without ReadBatch disables
// warming for the session after one attempt.
func TestWarmSiblings_UnsupportedSticky(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
	}
	p := &warmBatchProvider{files: files, batchErr: types.ErrBatchUnsupported}
	root := newWarmRoot(t, p, plainStat)

	root.maybeWarmSiblings("/evd/a.bin")
	waitFor(t, 5*time.Second, func() bool { return root.batchUnsupported.Load() })

	// A later trigger must not call the provider again.
	root.maybeWarmSiblings("/evd/a.bin")
	time.Sleep(100 * time.Millisecond)
	if got := p.batchCalls.Load(); got != 1 {
		t.Fatalf("batchCalls = %d, want 1 (unsupported must latch)", got)
	}
}

// TestWarmSiblings_PreserveMetadataApplied: with preserve_metadata on, a
// warmed file lands with the origin's mode and times.
func TestWarmSiblings_PreserveMetadataApplied(t *testing.T) {
	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	statFor := func(size int64) *winfuse.Stat_t {
		return &winfuse.Stat_t{
			Size: size,
			Mode: 0o640 | winfuse.S_IFREG,
			Mtim: winfuse.NewTimespec(origMtime),
			Atim: winfuse.NewTimespec(origAtime),
		}
	}
	files := map[string][]byte{
		"/evd/a.bin": makePattern(8 * 1024),
		"/evd/b.bin": makePattern(9 * 1024),
	}
	p := &warmBatchProvider{files: files}
	root := newWarmRoot(t, p, statFor)
	root.PreserveMetadata = true

	root.maybeWarmSiblings("/evd/a.bin")
	waitFor(t, 5*time.Second, warmSettled(root, "/evd"))

	diskPath := filepath.Join(root.LocalDownloadFolder, "/evd/b.bin")
	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("warmed file missing: %v", err)
	}
	if got := info.ModTime(); got.Sub(origMtime).Abs() > time.Second {
		t.Fatalf("disk mtime %v, want origin %v", got, origMtime)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o640 {
			t.Fatalf("disk mode %o, want 0640", got)
		}
	}
}
