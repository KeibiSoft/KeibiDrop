// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

// A dropped folder goes out in BatchNotify batches, not one Notify per file
// (a 12,789-file folder sent 12,789 RPCs).
func TestAddFilesAs_BatchesAnnouncements(t *testing.T) {
	dir := t.TempDir()
	count := announceBatchSize + 44
	items := make([]LocalAs, 0, count)
	for i := 0; i < count; i++ {
		local := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		writeFile(t, local, []byte("x"))
		items = append(items, LocalAs{Local: local, Remote: fmt.Sprintf("drop/f%04d.txt", i)})
	}

	kd, cli := newScanTestKD(t, t.TempDir())
	n, err := kd.AddFilesAs(items)
	require.NoError(t, err)
	require.Equal(t, count, n)
	require.Equal(t, 2, cli.batchCount())
	require.Zero(t, cli.singles, "a file went out on its own")
	paths := cli.paths()
	require.Len(t, paths, count)
	require.Equal(t, "drop/f0000.txt", paths[0])

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()
	require.Len(t, kd.SyncTracker.LocalFiles, count)
	require.Equal(t, items[5].Local, kd.SyncTracker.LocalFiles["drop/f0005.txt"].RealPathOfFile)
}

// The app's list reads the shared files while a share is in flight: neither
// the batch nor the single add may hold the list's lock across the round trip.
func TestAddFiles_ListReadableDuringTheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "a.txt")
	writeFile(t, local, []byte("alpha"))

	kd, cli := newScanTestKD(t, t.TempDir())
	readable := []bool{}
	cli.onSend = func() {
		ok := kd.SyncTracker.LocalFilesMu.TryRLock()
		if ok {
			kd.SyncTracker.LocalFilesMu.RUnlock()
		}
		readable = append(readable, ok)
	}
	_, err := kd.AddFilesAs([]LocalAs{{Local: local, Remote: "batch/a.txt"}})
	require.NoError(t, err)
	require.NoError(t, kd.AddFileAs(local, "single/a.txt"))
	require.Equal(t, []bool{true, true}, readable, "the list was locked during a send")
}

// A batch the peer never takes leaves nothing tracked, so a retry shares all.
func TestAddFilesAs_FailedBatchUntracksTheRest(t *testing.T) {
	dir := t.TempDir()
	items := []LocalAs{}
	for i := 0; i < 3; i++ {
		local := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		writeFile(t, local, []byte("x"))
		items = append(items, LocalAs{Local: local, Remote: filepath.Base(local)})
	}
	kd, cli := newScanTestKD(t, t.TempDir())
	withSharedStore(t, kd)
	cli.mu.Lock()
	cli.failNext = 1 << 30
	cli.mu.Unlock()

	n, err := kd.AddFilesAs(items)
	require.Error(t, err)
	require.Zero(t, n)
	kd.SyncTracker.LocalFilesMu.RLock()
	require.Empty(t, kd.SyncTracker.LocalFiles)
	kd.SyncTracker.LocalFilesMu.RUnlock()
	require.Empty(t, kd.sharedStore.Load(), "kept a share the friend never got")
}

// What a batch share sends is kept for this friend, as a single share is: the
// next session with them restores it (resilience.go). Without it a dropped
// folder was gone on both sides after a reconnect.
func TestAddFilesAs_KeepsTheSharesForTheNextSession(t *testing.T) {
	dir := t.TempDir()
	items := []LocalAs{}
	for i := 0; i < 3; i++ {
		local := filepath.Join(dir, fmt.Sprintf("f%d.txt", i))
		writeFile(t, local, []byte("x"))
		items = append(items, LocalAs{Local: local, Remote: "drop/" + filepath.Base(local)})
	}
	kd, _ := newScanTestKD(t, t.TempDir())
	withSharedStore(t, kd)

	n, err := kd.AddFilesAs(items)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	rels := []string{}
	for _, e := range kd.sharedStore.Load() {
		rels = append(rels, e.Rel)
	}
	require.ElementsMatch(t, []string{"drop/f0.txt", "drop/f1.txt", "drop/f2.txt"}, rels)
}

// A share stops when the session changes, maybe to another friend: the rest
// never goes to them, and what went out is kept for the first friend only.
func TestAddFilesAs_StopsWhenTheSessionChanges(t *testing.T) {
	dir := t.TempDir()
	count := announceBatchSize + 1
	items := make([]LocalAs, 0, count)
	for i := 0; i < count; i++ {
		local := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		writeFile(t, local, []byte("x"))
		items = append(items, LocalAs{Local: local, Remote: fmt.Sprintf("drop/f%04d.txt", i)})
	}
	kd, cli := newScanTestKD(t, t.TempDir())
	withSharedStore(t, kd)
	first := kd.session
	cli.onSend = func() {
		kd.mu.Lock()
		kd.session = &session.Session{GRPCClient: cli, ExpectedPeerFingerprint: "other-peer"}
		kd.mu.Unlock()
	}

	n, err := kd.AddFilesAs(items)
	require.Error(t, err)
	require.Equal(t, announceBatchSize, n)
	require.Equal(t, 1, cli.batchCount(), "the rest went out on the new session")
	entries := kd.sharedStore.Load()
	require.Len(t, entries, announceBatchSize)
	firstTag := kd.dlRegistry.peerTag(first.ExpectedPeerFingerprint, kd.registryKey)
	for _, e := range entries {
		require.Equal(t, firstTag, e.PeerTag)
	}
}

// Restored shares go back to the friend in batches, not one Notify each.
func TestNotifyRestoredFiles_Batches(t *testing.T) {
	dir := t.TempDir()
	kd, cli := newScanTestKD(t, t.TempDir())
	kd.KDClient = cli
	kd.ctx = context.Background()
	count := announceBatchSize + 3
	kd.SyncTracker.LocalFilesMu.Lock()
	for i := 0; i < count; i++ {
		local := filepath.Join(dir, fmt.Sprintf("f%04d.txt", i))
		writeFile(t, local, []byte("x"))
		rel := fmt.Sprintf("drop/f%04d.txt", i)
		kd.SyncTracker.LocalFiles[rel] = &synctracker.File{Name: filepath.Base(rel), RelativePath: rel, RealPathOfFile: local}
	}
	kd.SyncTracker.LocalFilesMu.Unlock()

	kd.notifyRestoredFiles(kd.logger)
	require.Equal(t, 2, cli.batchCount())
	require.Zero(t, cli.singles, "a file went out on its own")
	require.Len(t, cli.paths(), count)
}

func withSharedStore(t *testing.T, kd *KeibiDrop) {
	t.Helper()
	key := make([]byte, 32)
	kd.registryKey = key
	kd.dlRegistry = newDownloadRegistry(t.TempDir(), key)
	kd.sharedStore = newSharedFilesStore(t.TempDir(), key)
}

// A file that vanished between the walk and the share is skipped, not fatal.
func TestAddFilesAs_SkipsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.txt")
	writeFile(t, good, []byte("x"))
	kd, cli := newScanTestKD(t, t.TempDir())
	n, err := kd.AddFilesAs([]LocalAs{
		{Local: filepath.Join(dir, "gone.txt"), Remote: "gone.txt"},
		{Local: good, Remote: "good.txt"},
		{Local: dir, Remote: "a-folder"},
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"good.txt"}, cli.paths())
}
