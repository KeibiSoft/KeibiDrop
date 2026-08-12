// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests the desktop Notify handler: FUSE ADD/EDIT writing SyncTracker.RemoteFiles,
// ABOUTME: the no-FUSE ADD stale-guard, and the receiver-side REMOVE debounce.

//go:build !android

package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddFile_FuseMode_WritesSyncTracker verifies that when FUSE is active,
// a NotifyType_ADD_FILE request writes the file into SyncTracker.RemoteFiles
// so the UI can surface it via KD_GetFileCount/KD_GetFileName.
func TestAddFile_FuseMode_WritesSyncTracker(t *testing.T) {
	tmpDir := t.TempDir()

	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      tmpDir,
		LocalDownloadFolder: tmpDir,
		PrefetchSem:         make(chan struct{}, 8),
		// OpenStreamProvider returns nil — prefetchFile exits cleanly at fsp==nil check.
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)

	const (
		filePath = "/test-file.txt"
		fileName = "test-file.txt"
		fileSize = int64(1024)
	)

	req := &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE,
		Path: filePath,
		Name: fileName,
		Attr: &bindings.Attr{
			Size:             fileSize,
			ModificationTime: 1000000,
			BirthTime:        2000000,
			Mode:             0o644,
		},
	}

	resp, err := svc.Notify(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	got, ok := svc.SyncTracker.RemoteFiles[filePath]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.True(t, ok, "SyncTracker.RemoteFiles[%q] should be populated after FUSE ADD_FILE", filePath)
	assert.Equal(t, fileName, got.Name, "Name mismatch")
	assert.Equal(t, filePath, got.RelativePath, "RelativePath mismatch")
	assert.Equal(t, uint64(fileSize), got.Size, "Size mismatch")
}

// TestAddFile_NoFUSE_RejectsStaleSmallerOlder_AcceptsNewerShrink: the non-FUSE
// ADD path rejects a stale smaller ADD but accepts a newer shrink (git HEAD 25->21).
func TestAddFile_NoFUSE_RejectsStaleSmallerOlder_AcceptsNewerShrink(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const filePath = "HEAD"
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[filePath] = &synctracker.File{
		Name: filePath, RelativePath: filePath, Size: 25, LastEditTime: 1000,
	}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}
	sizeOf := func() uint64 {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		return st.RemoteFiles[filePath].Size
	}

	// (a) smaller AND older -> rejected, entry unchanged.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: filePath,
		Attr: &bindings.Attr{Size: 21, ModificationTime: 500},
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(25), sizeOf(), "stale smaller+older ADD must be rejected")

	// (b) smaller AND newer (legit shrink) -> accepted, entry updates.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: filePath,
		Attr: &bindings.Attr{Size: 21, ModificationTime: 2000},
	})
	require.NoError(t, err)
	assert.Equal(t, uint64(21), sizeOf(), "legit smaller+newer shrink must be accepted")

	// (c) resume entry has no stored mtime (LastEditTime==0); its size is the
	// stale one, so a fresh smaller ADD is accepted (else the resume would
	// target a stale-larger size and stall).
	st.RemoteFilesMu.Lock()
	st.RemoteFiles["resume"] = &synctracker.File{Name: "resume", RelativePath: "resume", Size: 25, LastEditTime: 0}
	st.RemoteFilesMu.Unlock()
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: "resume", Name: "resume",
		Attr: &bindings.Attr{Size: 21, ModificationTime: 2000},
	})
	require.NoError(t, err)
	st.RemoteFilesMu.RLock()
	assert.Equal(t, uint64(21), st.RemoteFiles["resume"].Size, "fresh smaller ADD on a resume entry (no stored mtime) must be accepted")
	st.RemoteFilesMu.RUnlock()
}

// TestEditFile_FuseMode_UpdatesSyncTracker verifies that when FUSE is active,
// a NotifyType_EDIT_FILE request updates an existing entry in SyncTracker.RemoteFiles.
func TestEditFile_FuseMode_UpdatesSyncTracker(t *testing.T) {
	tmpDir := t.TempDir()

	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      tmpDir,
		LocalDownloadFolder: tmpDir,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root

	tracker := synctracker.NewSyncTracker()

	const (
		filePath    = "/test-file.txt"
		fileName    = "test-file.txt"
		initialSize = uint64(1024)
		updatedSize = int64(2048)
		initialTime = uint64(1000000)
		updatedTime = uint64(3000000)
	)

	// Pre-populate SyncTracker with an existing entry.
	tracker.RemoteFiles[filePath] = &synctracker.File{
		Name:         fileName,
		RelativePath: filePath,
		Size:         initialSize,
		LastEditTime: initialTime,
	}

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: tracker,
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)

	req := &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE,
		Path: filePath,
		Name: fileName,
		Attr: &bindings.Attr{
			Size:             updatedSize,
			ModificationTime: updatedTime,
			BirthTime:        2000000,
			Mode:             0o644,
		},
	}

	resp, err := svc.Notify(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	tracker.RemoteFilesMu.RLock()
	got, ok := tracker.RemoteFiles[filePath]
	tracker.RemoteFilesMu.RUnlock()

	require.True(t, ok, "SyncTracker.RemoteFiles[%q] should exist after FUSE EDIT_FILE", filePath)
	assert.Equal(t, uint64(updatedSize), got.Size, "Size should be updated")
	assert.Equal(t, updatedTime, got.LastEditTime, "LastEditTime should be updated")
}

// TestEditFile_FuseMode_CreatesNewSyncTrackerEntry verifies that when FUSE is active,
// a NotifyType_EDIT_FILE for an unknown path creates a new SyncTracker entry.
func TestEditFile_FuseMode_CreatesNewSyncTrackerEntry(t *testing.T) {
	tmpDir := t.TempDir()

	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      tmpDir,
		LocalDownloadFolder: tmpDir,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)

	const (
		filePath    = "/new-file.txt"
		fileName    = "new-file.txt"
		fileSize    = int64(4096)
		fileModTime = uint64(5000000)
		fileBirth   = uint64(4000000)
	)

	req := &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE,
		Path: filePath,
		Name: fileName,
		Attr: &bindings.Attr{
			Size:             fileSize,
			ModificationTime: fileModTime,
			BirthTime:        fileBirth,
			Mode:             0o644,
		},
	}

	resp, err := svc.Notify(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	got, ok := svc.SyncTracker.RemoteFiles[filePath]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.True(t, ok, "SyncTracker.RemoteFiles[%q] should be created after FUSE EDIT_FILE on new path", filePath)
	assert.Equal(t, fileName, got.Name, "Name mismatch")
	assert.Equal(t, filePath, got.RelativePath, "RelativePath mismatch")
	assert.Equal(t, uint64(fileSize), got.Size, "Size mismatch")
	assert.Equal(t, fileModTime, got.LastEditTime, "LastEditTime mismatch")
	assert.Equal(t, fileBirth, got.CreatedTime, "CreatedTime mismatch")
}

// TestRemoveFile_NoFUSE_BufferedThenCancelledByAdd: REMOVE is buffered for
// 1000ms, not applied immediately; an ADD within the window cancels it.
func TestRemoveFile_NoFUSE_BufferedThenCancelledByAdd(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const filePath = "f.txt"
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[filePath] = &synctracker.File{Name: filePath, RelativePath: filePath, Size: 100}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}
	tracked := func() bool {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		_, ok := st.RemoteFiles[filePath]
		return ok
	}

	// REMOVE is buffered, not applied immediately.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)
	assert.True(t, tracked(), "REMOVE should be buffered, file still present")

	// An ADD for the same path within the window cancels the buffered remove.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: filePath,
		Attr: &bindings.Attr{Size: 100, ModificationTime: 1},
	})
	require.NoError(t, err)
	time.Sleep(1200 * time.Millisecond)
	assert.True(t, tracked(), "ADD within the window must cancel the buffered remove")
}

// TestRemoveFile_NoFUSE_ExecutesAfterWindow: a buffered REMOVE with no
// ADD/RENAME within the window executes and drops the tracked entry.
func TestRemoveFile_NoFUSE_ExecutesAfterWindow(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const filePath = "g.txt"
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[filePath] = &synctracker.File{Name: filePath, RelativePath: filePath, Size: 100}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)

	// Poll past the 1000ms window with a deadline so load cannot flake this.
	require.Eventually(t, func() bool {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		_, ok := st.RemoteFiles[filePath]
		return !ok
	}, 5*time.Second, 25*time.Millisecond, "buffered remove must execute after the window")
}

// TestRenameToFuseHidden_NoFUSE_AppliesAsRemoveOfSource: FUSE turns unlink of
// a still-open file into a rename to .fuse_hiddenN plus a later remove of the
// hidden name. The receiver must apply the rename as a buffered remove of the
// source, or the source stays forever (TextEdit .sb backup ghosts, 2026-08-11).
func TestRenameToFuseHidden_NoFUSE_AppliesAsRemoveOfSource(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const srcPath = "/graphify/doc.txt.sb-a9b1e7e9-wV07s3"
	const hiddenPath = "/graphify/.fuse_hidden000004c500000001"
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[srcPath] = &synctracker.File{Name: "doc.txt.sb-a9b1e7e9-wV07s3", RelativePath: srcPath, Size: 21}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}
	tracked := func(p string) bool {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		_, ok := st.RemoteFiles[p]
		return ok
	}

	// The rename to the hidden name buffers a remove of the source.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_RENAME_FILE, OldPath: srcPath, Path: hiddenPath,
	})
	require.NoError(t, err)
	assert.True(t, tracked(srcPath), "remove of the source must be buffered, not immediate")
	assert.False(t, tracked(hiddenPath), "the hidden name must never be tracked")

	// The follow-up remove of the hidden name stays dropped.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: hiddenPath,
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return !tracked(srcPath)
	}, 5*time.Second, 25*time.Millisecond, "rename to .fuse_hidden must remove the source")
	assert.False(t, tracked(hiddenPath), "the hidden name must never be tracked")
}

// TestRenameToFuseHidden_FuseMode_RemovesSourceEverywhere: in FUSE mode the
// translated remove must clear AllFileMap, RemoteFiles, SyncTracker and the
// cache file on disk.
func TestRenameToFuseHidden_FuseMode_RemovesSourceEverywhere(t *testing.T) {
	tmpDir := t.TempDir()

	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      tmpDir,
		LocalDownloadFolder: tmpDir,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)

	const srcPath = "/doc.txt.sb-a9b1e7e9-wV07s3"

	// Create the source through the real ADD path, plus its cache file.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: srcPath, Name: "doc.txt.sb-a9b1e7e9-wV07s3",
		Attr: &bindings.Attr{Size: 21, ModificationTime: 1000000, Mode: 0o644},
	})
	require.NoError(t, err)
	cachePath := filepath.Join(tmpDir, srcPath)
	require.NoError(t, os.WriteFile(cachePath, []byte("twenty one bytes here"), 0o644))

	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_RENAME_FILE, OldPath: srcPath, Path: "/.fuse_hidden000004e300000001",
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		root.AfmLock.RLock()
		_, inAfm := root.AllFileMap[srcPath]
		root.AfmLock.RUnlock()
		root.RemoteFilesLock.RLock()
		_, inRemote := root.RemoteFiles[srcPath]
		root.RemoteFilesLock.RUnlock()
		svc.SyncTracker.RemoteFilesMu.RLock()
		_, inTracker := svc.SyncTracker.RemoteFiles[srcPath]
		svc.SyncTracker.RemoteFilesMu.RUnlock()
		_, statErr := os.Stat(cachePath)
		return !inAfm && !inRemote && !inTracker && os.IsNotExist(statErr)
	}, 5*time.Second, 25*time.Millisecond, "source must leave AllFileMap, RemoteFiles, SyncTracker and disk")
}

// TestTextEditSafeSave_BatchSequence_NoGhostBackup replays the TextEdit
// safe-save wire sequence observed on 2026-08-11 (batch seq 146-148):
//  1. RENAME target -> .sb backup, RENAME untracked temp -> target
//  2. EDIT target, RENAME .sb backup -> .fuse_hiddenN (unlink while open)
//  3. REMOVE .fuse_hiddenN
//
// The target must survive with the new size, the .sb backup must go away,
// and the hidden name must never be tracked.
func TestTextEditSafeSave_BatchSequence_NoGhostBackup(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const (
		target  = "/graphify/lets_edit.txt"
		backup  = "/graphify/lets_edit.txt.sb-a9b1e7e9-q0myzN"
		hidden  = "/graphify/.fuse_hidden000004e300000001"
		tempSrc = "/.TemporaryItems/folders.501/TemporaryItems/NSIRD_TextEdit_N9FEPK/lets_edit.txt"
	)
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[target] = &synctracker.File{Name: "lets_edit.txt", RelativePath: target, Size: 21}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}
	sizeOf := func(p string) (uint64, bool) {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		f, ok := st.RemoteFiles[p]
		if !ok {
			return 0, false
		}
		return f.Size, true
	}

	// Batch 1: target renamed to the backup, temp renamed onto the target.
	resp, err := svc.BatchNotify(context.Background(), &bindings.BatchNotifyRequest{
		Seq: 146,
		Notifications: []*bindings.NotifyRequest{
			{Type: bindings.NotifyType_RENAME_FILE, OldPath: target, Path: backup,
				Attr: &bindings.Attr{Size: 21, ModificationTime: 1000}},
			{Type: bindings.NotifyType_RENAME_FILE, OldPath: tempSrc, Path: target,
				Attr: &bindings.Attr{Size: 9976, ModificationTime: 2000}},
		},
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, resp.Processed)

	// Batch 2: edit of the target, backup unlinked while open (hidden rename).
	resp, err = svc.BatchNotify(context.Background(), &bindings.BatchNotifyRequest{
		Seq: 147,
		Notifications: []*bindings.NotifyRequest{
			{Type: bindings.NotifyType_EDIT_FILE, Path: target, Name: "lets_edit.txt",
				Attr: &bindings.Attr{Size: 9976, ModificationTime: 2000}},
			{Type: bindings.NotifyType_RENAME_FILE, OldPath: backup, Path: hidden,
				Attr: &bindings.Attr{Size: 21, ModificationTime: 1000}},
		},
	})
	require.NoError(t, err)
	require.EqualValues(t, 2, resp.Processed)

	// Batch 3: remove of the hidden name, dropped as before.
	resp, err = svc.BatchNotify(context.Background(), &bindings.BatchNotifyRequest{
		Seq:           148,
		Notifications: []*bindings.NotifyRequest{{Type: bindings.NotifyType_REMOVE_FILE, Path: hidden}},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, resp.Processed)

	// Backup removal is buffered, target already carries the new size.
	_, backupTracked := sizeOf(backup)
	assert.True(t, backupTracked, "backup removal must be buffered, not immediate")
	gotSize, targetTracked := sizeOf(target)
	require.True(t, targetTracked, "target must stay tracked")
	assert.EqualValues(t, 9976, gotSize, "target must carry the new size")

	require.Eventually(t, func() bool {
		_, ok := sizeOf(backup)
		return !ok
	}, 5*time.Second, 25*time.Millisecond, "the .sb backup must not stay as a ghost")

	gotSize, targetTracked = sizeOf(target)
	require.True(t, targetTracked, "target must survive the backup removal")
	assert.EqualValues(t, 9976, gotSize, "target size must survive the backup removal")
	_, hiddenTracked := sizeOf(hidden)
	assert.False(t, hiddenTracked, "the hidden name must never be tracked")
}

// TestRemoveFile_NoFUSE_DisconnectCancelsPendingRemoves: a graceful DISCONNECT
// cancels buffered removes; the tracker outlives the session, so a surviving
// timer would delete a file re-synced by the next session.
func TestRemoveFile_NoFUSE_DisconnectCancelsPendingRemoves(t *testing.T) {
	st := synctracker.NewSyncTracker()
	const filePath = "h.txt"
	st.RemoteFilesMu.Lock()
	st.RemoteFiles[filePath] = &synctracker.File{Name: filePath, RelativePath: filePath, Size: 100}
	st.RemoteFilesMu.Unlock()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: st,
	}

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)

	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_DISCONNECT,
	})
	require.NoError(t, err)

	time.Sleep(1200 * time.Millisecond)
	st.RemoteFilesMu.RLock()
	_, ok := st.RemoteFiles[filePath]
	st.RemoteFilesMu.RUnlock()
	assert.True(t, ok, "DISCONNECT must cancel the buffered remove")
}
