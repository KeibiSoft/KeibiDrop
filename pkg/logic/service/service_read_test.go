// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

// ABOUTME: Tests for Read handler fallback to SyncTracker.LocalFiles when FUSE is active.
// ABOUTME: Regression test for drag-and-drop files not found by FUSE-mode Read handler.

package service

import (
	"os"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRead_FUSEMode_FallbackToLocalFiles verifies that when FUSE is active but a
// file was added via drag-and-drop (AddFile → SyncTracker.LocalFiles), the Read
// handler can still find and serve the file.
func TestRead_FUSEMode_FallbackToLocalFiles(t *testing.T) {
	// Create a temp file to serve as the "dropped" file.
	tmpFile, err := os.CreateTemp(t.TempDir(), "dropped-*.txt")
	require.NoError(t, err)
	content := []byte("hello from drag and drop")
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(&filesystem.Dir{
		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*filesystem.File),
	})
	svc.SetFS(fsForSvc)

	// Simulate AddFile: file goes into SyncTracker.LocalFiles only.
	svc.SyncTracker.LocalFiles["test.txt"] = &synctracker.File{
		Name:           "test.txt",
		RelativePath:   "test.txt",
		RealPathOfFile: tmpFile.Name(),
		Size:           uint64(len(content)),
	}

	stream := &testkit.Stream[*bindings.ReadRequest, *bindings.ReadResponse]{
		Requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "test.txt", Offset: 0, Size: uint32(len(content))},
		},
	}

	err = svc.Read(stream)
	require.NoError(t, err)
	require.Len(t, stream.Sent, 1)
	assert.Equal(t, content, stream.Sent[0].Data)
}

// TestRead_FUSEMode_AllFileMapStillWorks verifies the normal FUSE path still works
// (files in AllFileMap are found without needing the fallback).
func TestRead_FUSEMode_AllFileMapStillWorks(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "fuse-*.txt")
	require.NoError(t, err)
	content := []byte("file from FUSE mount")
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(&filesystem.Dir{
		AfmLock: sync.RWMutex{},
		AllFileMap: map[string]*filesystem.File{
			"/test.txt": {
				RealPathOfFile: tmpFile.Name(),
			},
		},
	})
	svc.SetFS(fsForSvc)

	stream := &testkit.Stream[*bindings.ReadRequest, *bindings.ReadResponse]{
		Requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "test.txt", Offset: 0, Size: uint32(len(content))},
		},
	}

	err = svc.Read(stream)
	require.NoError(t, err)
	require.Len(t, stream.Sent, 1)
	assert.Equal(t, content, stream.Sent[0].Data)
}

// TestRead_FUSEMode_FileNotFoundAnywhere verifies we still get NotFound when
// the file isn't in AllFileMap OR LocalFiles.
func TestRead_FUSEMode_FileNotFoundAnywhere(t *testing.T) {
	svc := &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(&filesystem.Dir{
		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*filesystem.File),
	})
	svc.SetFS(fsForSvc)

	stream := &testkit.Stream[*bindings.ReadRequest, *bindings.ReadResponse]{
		Requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "nonexistent.txt", Offset: 0, Size: 100},
		},
	}

	err := svc.Read(stream)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, st.Code())
}
