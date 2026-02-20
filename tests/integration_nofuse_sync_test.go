// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
)

// TestNoFUSE_RemoveFileReception verifies that when the peer sends a
// REMOVE_FILE notification, the file is removed from RemoteFiles.
func TestNoFUSE_RemoveFileReception(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Alice adds a file.
	content := []byte("file that will be removed")
	alicePath := filepath.Join(tp.AliceSaveDir, "removable.txt")
	require.NoError(os.WriteFile(alicePath, content, 0644))
	require.NoError(tp.Alice.AddFile(alicePath))

	// Bob should see it.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "removable.txt", 10*time.Second)

	// Simulate Alice sending REMOVE_FILE notification via gRPC.
	// In no-FUSE mode there's no RemoveFile() API method, so we call Notify directly.
	// tp.Alice.KDClient talks to Bob's gRPC server (Alice's client → Bob's Notify handler).
	_, err := tp.Alice.KDClient.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.RemoveFile),
		Path: "removable.txt",
	})
	require.NoError(err)

	// Bob should no longer see the file.
	WaitForCondition(t, 5*time.Second, 100*time.Millisecond, func() bool {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		_, exists := tp.Bob.SyncTracker.RemoteFiles["removable.txt"]
		return !exists
	}, "waiting for file to be removed from Bob's SyncTracker")
}

// TestNoFUSE_RenameFileReception verifies that when the peer sends a
// RENAME_FILE notification, the file is moved in RemoteFiles.
func TestNoFUSE_RenameFileReception(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Alice adds a file.
	content := []byte("file that will be renamed")
	alicePath := filepath.Join(tp.AliceSaveDir, "before_rename.txt")
	require.NoError(os.WriteFile(alicePath, content, 0644))
	require.NoError(tp.Alice.AddFile(alicePath))

	// Bob should see it.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "before_rename.txt", 10*time.Second)

	// Simulate Alice sending RENAME_FILE notification.
	// tp.Alice.KDClient talks to Bob's gRPC server.
	_, err := tp.Alice.KDClient.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType(types.RenameFile),
		Path:    "after_rename.txt",
		OldPath: "before_rename.txt",
	})
	require.NoError(err)

	// Bob should see the file under the new name.
	WaitForCondition(t, 5*time.Second, 100*time.Millisecond, func() bool {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		_, exists := tp.Bob.SyncTracker.RemoteFiles["after_rename.txt"]
		return exists
	}, "waiting for renamed file to appear in Bob's SyncTracker")

	// Old name should be gone.
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	_, oldExists := tp.Bob.SyncTracker.RemoteFiles["before_rename.txt"]
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	require.False(oldExists, "old file name should be removed after rename")
}

// TestNoFUSE_ErrorTypes verifies that specific error types are returned
// from AddFile and PullFile operations.
func TestNoFUSE_ErrorTypes(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	t.Run("AddFile_NonexistentPath", func(t *testing.T) {
		err := tp.Alice.AddFile("/nonexistent/path/file.txt")
		require.Error(err)
		// Should be a file-not-found error.
		require.True(os.IsNotExist(err), "expected os.IsNotExist, got: %v", err)
	})

	t.Run("AddFile_Directory", func(t *testing.T) {
		err := tp.Alice.AddFile(tp.AliceSaveDir)
		require.Error(err)
		require.ErrorIs(err, syscall.EISDIR)
	})

	t.Run("PullFile_Nonexistent", func(t *testing.T) {
		dest := filepath.Join(tp.BobSaveDir, "should_not_exist.txt")
		err := tp.Bob.PullFile("nonexistent_file.txt", dest)
		require.Error(err)
		// Should be ENOENT.
		require.ErrorIs(err, syscall.ENOENT)
	})

	t.Run("AddFile_Duplicate", func(t *testing.T) {
		content := []byte("dup test")
		path := filepath.Join(tp.AliceSaveDir, "dup_error.txt")
		require.NoError(os.WriteFile(path, content, 0644))
		require.NoError(tp.Alice.AddFile(path))

		// Second add should return os.ErrExist.
		err := tp.Alice.AddFile(path)
		require.ErrorIs(err, os.ErrExist)
	})
}
