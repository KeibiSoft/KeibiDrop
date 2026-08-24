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
	"math"
	"os"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
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

// TestRead_HostileSizeNeverExceedsBuffer verifies the real Read call site
// (T4: clampReadSize) does not panic when a peer requests a Size far larger
// than the serve buffer, and that the response never exceeds the buffer.
// ReadRequest.Size is uint32, so int(rec.Size) cannot go negative on this
// amd64 host; only the size>bufLen bound is reachable here. The negative-wrap
// case (32-bit builds) is covered by TestClampReadSize in readsize_test.go.
func TestRead_HostileSizeNeverExceedsBuffer(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "hostile-*.txt")
	require.NoError(t, err)
	content := []byte("small file, huge requested size")
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

	svc.SyncTracker.LocalFiles["hostile.txt"] = &synctracker.File{
		Name:           "hostile.txt",
		RelativePath:   "hostile.txt",
		RealPathOfFile: tmpFile.Name(),
		Size:           uint64(len(content)),
	}

	stream := &testkit.Stream[*bindings.ReadRequest, *bindings.ReadResponse]{
		Requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "hostile.txt", Offset: 0, Size: math.MaxUint32},
		},
	}

	require.NotPanics(t, func() {
		err = svc.Read(stream)
	})
	require.NoError(t, err)
	require.Len(t, stream.Sent, 1)
	assert.LessOrEqual(t, len(stream.Sent[0].Data), config.GRPCStreamBuffer,
		"response must never exceed the serve buffer")
	assert.Equal(t, content, stream.Sent[0].Data,
		"small file must still be served in full despite the hostile size")
}

// TestRead_HostileOffsetRejected verifies the real Read call site refuses an offset
// that cannot be represented as int64. rec.Offset is uint64, so 1<<63 wraps negative
// and would otherwise reach ReadAt and leave the open handle behind.
func TestRead_HostileOffsetRejected(t *testing.T) {
	tmpFile, err := os.CreateTemp(t.TempDir(), "offset-*.txt")
	require.NoError(t, err)
	_, err = tmpFile.Write([]byte("payload"))
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
	svc.SyncTracker.LocalFiles["offset.txt"] = &synctracker.File{
		Name:           "offset.txt",
		RelativePath:   "offset.txt",
		RealPathOfFile: tmpFile.Name(),
		Size:           7,
	}

	stream := &testkit.Stream[*bindings.ReadRequest, *bindings.ReadResponse]{
		Requests: []*bindings.ReadRequest{
			{Handle: 0, Path: "offset.txt", Offset: 1 << 63, Size: 1},
		},
	}

	require.NotPanics(t, func() { err = svc.Read(stream) })
	require.Error(t, err, "an unrepresentable offset must be refused")
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code(), "must be InvalidArgument, not Internal")
	assert.Empty(t, stream.Sent, "nothing may be served for a rejected offset")
}
