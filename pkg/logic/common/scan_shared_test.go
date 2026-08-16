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
	mu      sync.Mutex
	reqs    []*bindings.NotifyRequest
	batches int
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
