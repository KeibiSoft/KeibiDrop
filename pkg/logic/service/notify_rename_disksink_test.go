// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests the RENAME_FILE disk sink (os.MkdirAll/RenameShared/os.Remove
// ABOUTME: in Notify) directly, mount-free, including the T3 path-safety guard.

//go:build !android

package service

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRenameDiskSinkTestService builds a KeibidropServiceImpl whose FS root
// points at a real temp dir, without mounting FUSE: SetRoot wires a
// hand-built tree, matching the pattern notify_synctracker_test.go uses.
func newRenameDiskSinkTestService(realRoot string) *KeibidropServiceImpl {
	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      realRoot,
		LocalDownloadFolder: realRoot,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)
	return svc
}

// TestNotify_RenameFile_MovesRealFileWithinRoot drives a legit RENAME_FILE
// Notify through the real disk sink (service.go's RENAME_FILE branch) and
// asserts the file actually moves on disk, under the temp root.
func TestNotify_RenameFile_MovesRealFileWithinRoot(t *testing.T) {
	tmpDir := t.TempDir()
	svc := newRenameDiskSinkTestService(tmpDir)

	const (
		oldPath = "/src/doc.txt"
		newPath = "/dst/renamed.txt"
	)
	oldDisk := filepath.Join(tmpDir, oldPath)
	newDisk := filepath.Join(tmpDir, newPath)
	const payload = "legit rename payload"

	require.NoError(t, os.MkdirAll(filepath.Dir(oldDisk), 0o755))
	require.NoError(t, os.WriteFile(oldDisk, []byte(payload), 0o644))

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: oldPath,
		Path:    newPath,
	})
	require.NoError(t, err)

	_, statErr := os.Stat(oldDisk)
	assert.True(t, os.IsNotExist(statErr), "source must no longer exist after rename")

	got, err := os.ReadFile(newDisk)
	require.NoError(t, err, "renamed file must exist at the new real path")
	assert.Equal(t, payload, string(got))
}

// TestNotify_RenameFile_RefusesUnsafePath_NoDiskEscape drives a RENAME_FILE
// Notify carrying ".." in Path or OldPath and asserts (T3) it is refused
// with ErrGRPCUnsafePath before any disk I/O, leaving the outside path
// untouched and the legit source file exactly where it was.
func TestNotify_RenameFile_RefusesUnsafePath_NoDiskEscape(t *testing.T) {
	tmpDir := t.TempDir()
	svc := newRenameDiskSinkTestService(tmpDir)

	const legitPath = "/source.txt"
	const payload = "must stay put"
	legitDisk := filepath.Join(tmpDir, legitPath)
	require.NoError(t, os.WriteFile(legitDisk, []byte(payload), 0o644))

	// What the disk path would have been had the guard not fired:
	// Clean(Join(tmpDir, "../escaped.txt")) climbs one level above the root.
	outsideTarget := filepath.Join(filepath.Dir(tmpDir), "escaped.txt")
	defer os.Remove(outsideTarget) // safety net; must be a no-op if the guard holds

	// Path carries "..": refused before any disk I/O.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: legitPath,
		Path:    "../escaped.txt",
	})
	require.ErrorIs(t, err, ErrGRPCUnsafePath)

	// OldPath carries "..": also refused before any disk I/O.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: "../escaped.txt",
		Path:    "/renamed.txt",
	})
	require.ErrorIs(t, err, ErrGRPCUnsafePath)

	_, statErr := os.Stat(outsideTarget)
	assert.True(t, os.IsNotExist(statErr), "unsafe rename must not write outside the save root")

	got, err := os.ReadFile(legitDisk)
	require.NoError(t, err, "legit source file must be untouched by the refused attempts")
	assert.Equal(t, payload, string(got))
}

// A rename whose OldPath resolves to the save root must never reach the disk sink:
// on an empty root os.Remove(root) would succeed and delete the user's save folder.
func TestNotify_RenameFile_RefusesRootAsOldPath(t *testing.T) {
	for _, oldPath := range []string{"", "/", ".", "./", "/. ", " "} {
		t.Run("oldPath="+oldPath, func(t *testing.T) {
			tmpDir := t.TempDir()
			root := filepath.Join(tmpDir, "root")
			require.NoError(t, os.Mkdir(root, 0o755))
			svc := newRenameDiskSinkTestService(root)

			_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
				Type:    bindings.NotifyType_RENAME_FILE,
				OldPath: oldPath,
				Path:    "moved.txt",
			})

			require.ErrorIs(t, err, ErrGRPCUnsafePath, "a rename off the save root must be refused")
			info, statErr := os.Stat(root)
			require.NoError(t, statErr, "the save root must still exist")
			assert.True(t, info.IsDir(), "the save root must still be a directory")
		})
	}
}
