// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests that ADD_FILE and EDIT_FILE via the FUSE path write SyncTracker.RemoteFiles.
// ABOUTME: Regression for FUSE-mode files being invisible in the UI via KD_GetFileCount/KD_GetFileName.

//go:build !android

package service

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
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
		FsCtx:               context.Background(),
		PrefetchSem:         make(chan struct{}, 8),
		// OpenStreamProvider returns nil — prefetchFile exits cleanly at fsp==nil check.
		OpenStreamProvider: func() types.FileStreamProvider { return nil },
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: root,
		},
		SyncTracker: synctracker.NewSyncTracker(),
	}

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
		FsCtx:               context.Background(),
		PrefetchSem:         make(chan struct{}, 8),
		OpenStreamProvider:  func() types.FileStreamProvider { return nil },
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
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: root,
		},
		SyncTracker: tracker,
	}

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
		FsCtx:               context.Background(),
		PrefetchSem:         make(chan struct{}, 8),
		OpenStreamProvider:  func() types.FileStreamProvider { return nil },
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		FS: &filesystem.FS{
			Root: root,
		},
		SyncTracker: synctracker.NewSyncTracker(),
	}

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
