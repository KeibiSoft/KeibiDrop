// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests the receiver remove-buffer race: a local write landing inside the
// ABOUTME: 1000ms REMOVE window must survive; a genuine remove must still execute.

//go:build !android

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func newFuseServiceForRemoveTests(t *testing.T) (*KeibidropServiceImpl, *filesystem.Dir, string) {
	t.Helper()
	tmpDir := t.TempDir()
	root := filesystem.NewBareRoot(tmpDir)
	svc := &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)
	return svc, root, tmpDir
}

// TestRemoveFile_FuseMode_LocalWriteDuringWindowSurvives reproduces the
// remove-buffer race: a peer REMOVE arms the 1000ms window, a local write
// lands and closes inside it, and the fired timer must NOT delete the bytes.
func TestRemoveFile_FuseMode_LocalWriteDuringWindowSurvives(t *testing.T) {
	svc, root, tmpDir := newFuseServiceForRemoveTests(t)
	const filePath = "/doc.txt"
	root.SetStreamProvider(func() types.FileStreamProvider { return &sliceProvider{data: []byte("original")} })

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: "doc.txt",
		Attr: &bindings.Attr{Size: 8, ModificationTime: 1000000, Mode: 0o644},
	})
	require.NoError(t, err)
	cachePath := filepath.Join(tmpDir, filePath)
	require.NoError(t, os.WriteFile(cachePath, []byte("original"), 0o644))

	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)

	// The local write lands through the mount and closes well inside the
	// 1000ms window. With FUSE every local write takes this path; a write
	// straight into the cache file is what a cache fill looks like.
	time.Sleep(50 * time.Millisecond)
	var st winfuse.Stat_t
	require.Equal(t, 0, root.Getattr(filePath, &st, 0))
	fi := &winfuse.FileInfo_t{Flags: os.O_WRONLY}
	require.Equal(t, 0, root.OpenEx(filePath, fi))
	require.Equal(t, 8, root.Write(filePath, []byte("editlocl"), 0, fi.Fh))
	require.Equal(t, 0, root.Release(filePath, fi.Fh))

	// Let the buffered remove fire; the local bytes must still be there.
	time.Sleep(1300 * time.Millisecond)
	got, err := os.ReadFile(cachePath)
	require.NoError(t, err, "local write inside the remove window must survive")
	require.Equal(t, "editlocl", string(got))
}

// TestRemoveFile_FuseMode_GenuineRemoveStillExecutes: with no local activity
// after the peer REMOVE, the buffered remove must still delete file and state.
func TestRemoveFile_FuseMode_GenuineRemoveStillExecutes(t *testing.T) {
	svc, root, tmpDir := newFuseServiceForRemoveTests(t)
	const filePath = "/gone.txt"

	cachePath := filepath.Join(tmpDir, filePath)
	require.NoError(t, os.WriteFile(cachePath, []byte("stale"), 0o644))
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: "gone.txt",
		Attr: &bindings.Attr{Size: 5, ModificationTime: 1000000, Mode: 0o644},
	})
	require.NoError(t, err)

	// The file's mtime predates the REMOVE arming below.
	time.Sleep(50 * time.Millisecond)
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)

	testkit.Eventually(t, 5*time.Second, 25*time.Millisecond, func() bool {
		root.AfmLock.RLock()
		_, inAfm := root.AllFileMap[filePath]
		root.AfmLock.RUnlock()
		_, statErr := os.Stat(cachePath)
		return !inAfm && os.IsNotExist(statErr)
	}, "genuine buffered remove must still execute")
}
