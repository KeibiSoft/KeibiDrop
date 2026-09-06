// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests for ScanAndShareSaveDir — announce pre-existing save dir files.
// ABOUTME: Covers idempotence, internal-file skip, nested paths, BFS order, remote skip.

package common

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeNotifyCli records Notify and BatchNotify requests; other RPCs come from
// the embedded nil interface and panic if called.
type fakeNotifyCli struct {
	bindings.KeibiServiceClient
	mu       sync.Mutex
	reqs     []*bindings.NotifyRequest
	batches  int
	failNext int // BatchNotify calls to fail before succeeding again
}

func (f *fakeNotifyCli) Notify(_ context.Context, req *bindings.NotifyRequest, _ ...grpc.CallOption) (*bindings.NotifyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	return &bindings.NotifyResponse{}, nil
}

func (f *fakeNotifyCli) BatchNotify(_ context.Context, req *bindings.BatchNotifyRequest, _ ...grpc.CallOption) (*bindings.BatchNotifyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	if f.failNext > 0 {
		f.failNext--
		return nil, fmt.Errorf("simulated send failure")
	}
	f.reqs = append(f.reqs, req.Notifications...)
	return &bindings.BatchNotifyResponse{Status: "ok", Processed: uint32(len(req.Notifications))}, nil
}

func (f *fakeNotifyCli) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		out = append(out, r.Path)
	}
	return out
}

func (f *fakeNotifyCli) batchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.batches
}

func newScanTestKD(t *testing.T, saveDir string) (*KeibiDrop, *fakeNotifyCli) {
	t.Helper()
	cli := &fakeNotifyCli{}
	kd := newBareKD()
	kd.SyncTracker = synctracker.NewSyncTracker()
	kd.ToSave = saveDir
	kd.session = &session.Session{
		GRPCClient:              cli,
		ExpectedPeerFingerprint: "test-peer",
	}
	return kd, cli
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, content, 0o644))
}

func TestScanAndShareSaveDir_AnnouncesPreExistingFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))
	writeFile(t, filepath.Join(dir, "b.bin"), []byte("beta"))
	writeFile(t, filepath.Join(dir, "sub", "c.txt"), []byte("gamma"))
	writeFile(t, filepath.Join(dir, ".DS_Store"), []byte("junk"))
	writeFile(t, filepath.Join(dir, "b.bin.kdbitmap"), []byte("sidecar"))

	kd, cli := newScanTestKD(t, dir)
	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, n)

	paths := cli.paths()
	require.ElementsMatch(t, []string{"a.txt", "b.bin", "sub/c.txt"}, paths)
	// Breadth-first: both top-level files announce before the nested one.
	require.Equal(t, "sub/c.txt", paths[2])

	for _, req := range cli.reqs {
		require.Equal(t, bindings.NotifyType_ADD_FILE, req.Type)
		require.NotNil(t, req.Attr)
		require.NotZero(t, req.Attr.ModificationTime)
	}

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	require.Len(t, kd.SyncTracker.LocalFiles, 3)
	require.Contains(t, kd.SyncTracker.LocalFiles, "sub/c.txt")
	require.Equal(t, "sub/c.txt", kd.SyncTracker.LocalFiles["sub/c.txt"].RelativePath)
	require.NotContains(t, kd.SyncTracker.LocalFiles, ".DS_Store")
}

func TestScanAndShareSaveDir_Idempotent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))

	kd, cli := newScanTestKD(t, dir)
	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)

	n, err = kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, cli.paths(), 1)
}

func TestScanAndShareSaveDir_SkipsPeerOwnedFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "theirs.txt"), []byte("from peer"))
	writeFile(t, filepath.Join(dir, "mine.txt"), []byte("mine"))

	kd, cli := newScanTestKD(t, dir)
	kd.SyncTracker.RemoteFiles["theirs.txt"] = &synctracker.File{
		Name: "theirs.txt", RelativePath: "theirs.txt",
	}

	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"mine.txt"}, cli.paths())
}

func TestScanAndShareSaveDir_NoSession(t *testing.T) {
	kd := newBareKD()
	kd.SyncTracker = synctracker.NewSyncTracker()
	kd.ToSave = t.TempDir()
	_, err := kd.ScanAndShareSaveDir(context.Background())
	require.ErrorIs(t, err, ErrInvalidSession)
}

func TestScanAndShareSaveDir_MissingDir(t *testing.T) {
	kd, _ := newScanTestKD(t, filepath.Join(t.TempDir(), "gone"))
	_, err := kd.ScanAndShareSaveDir(context.Background())
	require.Error(t, err)
}

func TestScanAndShareSaveDir_CapsAnnounceBatches(t *testing.T) {
	dir := t.TempDir()
	count := announceBatchSize + 10
	for i := 0; i < count; i++ {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte("x"))
	}

	kd, cli := newScanTestKD(t, dir)
	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, count, n)
	require.Equal(t, 2, cli.batchCount())
	require.Len(t, cli.paths(), count)
}

func TestScanAndShareSaveDir_PersistsRelPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))
	writeFile(t, filepath.Join(dir, "sub", "c.txt"), []byte("gamma"))

	kd, _ := newScanTestKD(t, dir)
	key := make([]byte, 32)
	kd.registryKey = key
	kd.dlRegistry = newDownloadRegistry(t.TempDir(), key)
	kd.sharedStore = newSharedFilesStore(t.TempDir(), key)

	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)

	entries := kd.sharedStore.Load()
	require.Len(t, entries, 2)
	rels := []string{entries[0].Rel, entries[1].Rel}
	require.ElementsMatch(t, []string{"a.txt", "sub/c.txt"}, rels)
}

// One failed BatchNotify send must not silence the rest of the scan.
func TestScanAndShareSaveDir_RetriesFailedBatch(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))
	writeFile(t, filepath.Join(dir, "b.txt"), []byte("beta"))

	kd, cli := newScanTestKD(t, dir)
	cli.mu.Lock()
	cli.failNext = 1
	cli.mu.Unlock()

	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, n)
	require.ElementsMatch(t, []string{"a.txt", "b.txt"}, cli.paths())
}

// When every retry fails, the batch's files are untracked again, so a later
// scan announces them instead of skipping them as already tracked.
func TestScanAndShareSaveDir_FailedBatchLeftUntracked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))

	kd, cli := newScanTestKD(t, dir)
	cli.mu.Lock()
	cli.failNext = 1 << 30
	cli.mu.Unlock()

	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.Error(t, err)
	require.Zero(t, n)

	kd.SyncTracker.LocalFilesMu.RLock()
	require.Empty(t, kd.SyncTracker.LocalFiles)
	kd.SyncTracker.LocalFilesMu.RUnlock()

	// The connection heals: the same scan now announces everything.
	cli.mu.Lock()
	cli.failNext = 0
	cli.mu.Unlock()
	n, err = kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.ElementsMatch(t, []string{"a.txt"}, cli.paths())
}

func TestScanAndShareSaveDir_CancelledContext(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("alpha"))

	kd, cli := newScanTestKD(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := kd.ScanAndShareSaveDir(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, cli.paths())
}

// A file whose mtime is still moving is skipped while rescans are on, and
// announced once it has settled. With rescans off every file is announced at
// once, because nothing would come back for it.
func TestScanAndShareSaveDir_SettleWindow(t *testing.T) {
	dir := t.TempDir()
	kd, cli := newScanTestKD(t, dir)
	kd.RescanSharedSeconds = 30
	writeFile(t, filepath.Join(dir, "landing.mov"), []byte("partial"))

	n, err := kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, n, "a file written just now must wait for its mtime to settle")

	old := time.Now().Add(-2 * sharedSettleWindow)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "landing.mov"), old, old))
	n, err = kd.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"landing.mov"}, cli.paths())

	kd2, cli2 := newScanTestKD(t, t.TempDir())
	kd2.RescanSharedSeconds = 0
	writeFile(t, filepath.Join(kd2.ToSave, "now.mov"), []byte("x"))
	n, err = kd2.ScanAndShareSaveDir(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, n, "with rescans off a fresh file is announced at once")
	require.Equal(t, []string{"now.mov"}, cli2.paths())
}

// The rescan loop announces a file that lands after the session started, and
// stops when its context ends.
func TestRescanSharedLoop_AnnouncesLateFiles(t *testing.T) {
	dir := t.TempDir()
	kd, cli := newScanTestKD(t, dir)
	kd.RescanSharedSeconds = 1
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		kd.rescanSharedLoop(ctx, kd.logger)
		close(done)
	}()

	late := filepath.Join(dir, "late.mov")
	writeFile(t, late, []byte("late"))
	settled := time.Now().Add(-2 * sharedSettleWindow)
	require.NoError(t, os.Chtimes(late, settled, settled))

	require.Eventually(t, func() bool {
		for _, p := range cli.paths() {
			if p == "late.mov" {
				return true
			}
		}
		return false
	}, 5*time.Second, 100*time.Millisecond, "the rescan loop must announce a late file")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("rescan loop did not stop on context cancel")
	}
}

// A newer loop retires the older one, so a reconnect never leaves two walkers.
func TestRescanSharedLoop_NewerGenerationRetiresOlder(t *testing.T) {
	kd, _ := newScanTestKD(t, t.TempDir())
	kd.RescanSharedSeconds = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan struct{})
	go func() {
		kd.rescanSharedLoop(ctx, kd.logger)
		close(first)
	}()
	time.Sleep(50 * time.Millisecond)
	go kd.rescanSharedLoop(ctx, kd.logger) // the reconnect's loop
	select {
	case <-first:
	case <-time.After(4 * time.Second):
		t.Fatal("the older rescan loop must exit once a newer one starts")
	}
}

// Off by default in the engine: a zero interval returns at once.
func TestRescanSharedLoop_ZeroIntervalReturns(t *testing.T) {
	kd, _ := newScanTestKD(t, t.TempDir())
	kd.RescanSharedSeconds = 0
	done := make(chan struct{})
	go func() {
		kd.rescanSharedLoop(context.Background(), kd.logger)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a zero interval must not start a loop")
	}
}

// A disconnect while CreateRoom waits for the peer's fingerprint returns at
// once instead of holding the daemon until Timeout.
func TestWaitForPeerFingerprint_CancelledByDisconnect(t *testing.T) {
	kd, _ := newScanTestKD(t, t.TempDir())
	kd.session.ExpectedPeerFingerprint = ""
	errCh := make(chan error, 1)
	go func() { errCh <- kd.waitForPeerFingerprint() }()
	time.Sleep(100 * time.Millisecond)
	kd.CancelPendingConnect()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrConnectCancelled)
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return after CancelPendingConnect")
	}
}
