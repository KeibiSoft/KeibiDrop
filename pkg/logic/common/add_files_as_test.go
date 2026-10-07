// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
	cli.mu.Lock()
	cli.failNext = 1 << 30
	cli.mu.Unlock()

	n, err := kd.AddFilesAs(items)
	require.Error(t, err)
	require.Zero(t, n)
	kd.SyncTracker.LocalFilesMu.RLock()
	require.Empty(t, kd.SyncTracker.LocalFiles)
	kd.SyncTracker.LocalFilesMu.RUnlock()
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
