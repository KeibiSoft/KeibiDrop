// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: A REMOVE_FILE from a web or no-FUSE peer carries a bare name; the FUSE maps key
// ABOUTME: by "/", so the remove must still clear the entry and the cache file.
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
	"github.com/stretchr/testify/require"
)

func TestRemoveFile_FuseMode_BarePathClearsFuseEntry(t *testing.T) {
	svc, root, tmpDir := newFuseServiceForRemoveTests(t)
	const bare = "gone.txt" // as the web peer sends it
	cachePath := filepath.Join(tmpDir, bare)
	require.NoError(t, os.WriteFile(cachePath, []byte("stale"), 0o644))

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: bare, Name: bare,
		Attr: &bindings.Attr{Size: 5, ModificationTime: 1000000, Mode: 0o644},
	})
	require.NoError(t, err)
	root.RemoteFilesLock.RLock()
	_, keyed := root.RemoteFiles["/"+bare]
	root.RemoteFilesLock.RUnlock()
	require.True(t, keyed, "ADD keys the FUSE map (what Readdir lists) with a leading slash")

	time.Sleep(50 * time.Millisecond) // the cache mtime predates the REMOVE arming
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: bare,
	})
	require.NoError(t, err)

	testkit.Eventually(t, 5*time.Second, 25*time.Millisecond, func() bool {
		root.RemoteFilesLock.RLock()
		_, listed := root.RemoteFiles["/"+bare]
		root.RemoteFilesLock.RUnlock()
		root.AfmLock.RLock()
		_, inAfm := root.AllFileMap["/"+bare]
		root.AfmLock.RUnlock()
		_, statErr := os.Stat(cachePath)
		return !listed && !inAfm && os.IsNotExist(statErr)
	}, "a bare-path REMOVE must clear the FUSE entry and the cache file")
}
