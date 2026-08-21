// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration tests for the ReadBatch RPC.
// ABOUTME: Frame reassembly, per-file errors, budget stop, read-only origin.

package tests

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// batchFrames drains a ReadBatch stream and returns the frames per path, in
// arrival order, plus the order in which paths first appeared.
func batchFrames(t *testing.T, stream bindings.KeibiService_ReadBatchClient) (map[string][]*bindings.ReadBatchResponse, []string) {
	t.Helper()
	frames := make(map[string][]*bindings.ReadBatchResponse)
	var order []string
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return frames, order
		}
		require.NoError(t, err)
		if _, seen := frames[resp.Path]; !seen {
			order = append(order, resp.Path)
		}
		frames[resp.Path] = append(frames[resp.Path], resp)
	}
}

// reassemble concatenates a path's data frames and checks frame invariants.
func reassemble(t *testing.T, frames []*bindings.ReadBatchResponse) []byte {
	t.Helper()
	var buf []byte
	for i, f := range frames {
		require.Zero(t, f.ErrCode, "unexpected err_code")
		require.Equal(t, uint64(len(buf)), f.Offset, "frames must arrive in order")
		buf = append(buf, f.Data...)
		require.Equal(t, i == len(frames)-1, f.FileDone, "file_done only on the last frame")
	}
	if len(frames) > 0 {
		require.Equal(t, frames[0].TotalSize, uint64(len(buf)), "reassembled size must match total_size")
	}
	return buf
}

// TestReadBatch_FrameReassembly: several files in one batch, one missing path
// in the middle, one file large enough to need two frames.
func TestReadBatch_FrameReassembly(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	sizes := map[string]int{
		"small_a.bin":  15 * 1024,
		"small_b.bin":  700,
		"twoframe.bin": 600 * 1024, // Above the 512 KiB frame cap.
		"small_c.bin":  4096,
	}
	content := make(map[string][]byte)
	for name, size := range sizes {
		data := make([]byte, size)
		_, err := rand.Read(data)
		require.NoError(err)
		content[name] = data
		p := filepath.Join(tp.BobSaveDir, name)
		require.NoError(os.WriteFile(p, data, 0o644))
		require.NoError(tp.Bob.AddFile(p))
	}
	for name := range sizes {
		WaitForRemoteFile(t, tp.Alice.SyncTracker, name, 10*time.Second)
	}

	req := &bindings.ReadBatchRequest{
		Paths:    []string{"small_a.bin", "missing.bin", "twoframe.bin", "small_b.bin", "small_c.bin"},
		MaxBytes: 4 * 1024 * 1024,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := tp.Alice.KDClient.ReadBatch(ctx, req)
	require.NoError(err)
	frames, order := batchFrames(t, stream)

	require.Equal(req.Paths, order, "files must arrive in requested order")

	miss := frames["missing.bin"]
	require.Len(miss, 1)
	require.EqualValues(1, miss[0].ErrCode, "missing file must report err_code 1")
	require.True(miss[0].FileDone)

	for name, want := range content {
		got := reassemble(t, frames[name])
		require.Equal(want, got, "bytes must be exact for %s", name)
	}
	require.Len(frames["twoframe.bin"], 2, "600 KiB must arrive as two frames")
}

// TestReadBatch_BudgetSoftStop: the budget stops NEW files, never truncates
// the current one.
func TestReadBatch_BudgetSoftStop(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	var names []string
	content := make(map[string][]byte)
	for i := range 3 {
		name := fmt.Sprintf("budget_%d.bin", i)
		names = append(names, name)
		data := make([]byte, 300*1024)
		_, err := rand.Read(data)
		require.NoError(err)
		content[name] = data
		p := filepath.Join(tp.BobSaveDir, name)
		require.NoError(os.WriteFile(p, data, 0o644))
		require.NoError(tp.Bob.AddFile(p))
	}
	for _, name := range names {
		WaitForRemoteFile(t, tp.Alice.SyncTracker, name, 10*time.Second)
	}

	// 350 KiB budget: file 0 fits, file 1 starts because the budget is not yet
	// spent and finishes whole, file 2 must not start.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := tp.Alice.KDClient.ReadBatch(ctx, &bindings.ReadBatchRequest{
		Paths:    names,
		MaxBytes: 350 * 1024,
	})
	require.NoError(err)
	frames, _ := batchFrames(t, stream)

	require.Equal(content[names[0]], reassemble(t, frames[names[0]]))
	require.Equal(content[names[1]], reassemble(t, frames[names[1]]), "current file must finish whole past the budget")
	require.Empty(frames[names[2]], "no new file starts past the budget")
}

// TestReadBatch_PathCap: more than the path cap is refused.
func TestReadBatch_PathCap(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	paths := make([]string, 513)
	for i := range paths {
		paths[i] = fmt.Sprintf("nope_%d.bin", i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stream, err := tp.Alice.KDClient.ReadBatch(ctx, &bindings.ReadBatchRequest{Paths: paths})
	require.NoError(err)
	_, err = stream.Recv()
	require.Error(err)
	require.Equal(codes.InvalidArgument, status.Code(err))
}

// TestReadBatch_ReadOnlyOriginUntouched: a share_read_only origin serves the
// batch and its files stay byte and mtime identical.
func TestReadBatch_ReadOnlyOriginUntouched(t *testing.T) {
	require := require.New(t)

	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	names := []string{"ev_a.bin", "ev_b.bin", "ev_c.bin"}
	content := make(map[string][]byte)
	tp := setupPeerPairImpl(t, false, false, 60*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			for _, name := range names {
				data := make([]byte, 20*1024)
				_, err := rand.Read(data)
				require.NoError(err)
				content[name] = data
				p := filepath.Join(bobSave, name)
				require.NoError(os.WriteFile(p, data, 0o644))
				require.NoError(os.Chtimes(p, old, old))
			}
			bob.ScanSharedOnStart = true
			bob.ShareReadOnly = true
		})

	for _, name := range names {
		WaitForRemoteFile(t, tp.Alice.SyncTracker, name, 15*time.Second)
	}

	before := make(map[string]os.FileInfo)
	for _, name := range names {
		info, err := os.Stat(filepath.Join(tp.BobSaveDir, name))
		require.NoError(err)
		before[name] = info
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := tp.Alice.KDClient.ReadBatch(ctx, &bindings.ReadBatchRequest{Paths: names, MaxBytes: 4 * 1024 * 1024})
	require.NoError(err)
	frames, _ := batchFrames(t, stream)
	for _, name := range names {
		require.Equal(content[name], reassemble(t, frames[name]))
	}

	for _, name := range names {
		info, err := os.Stat(filepath.Join(tp.BobSaveDir, name))
		require.NoError(err)
		require.Equal(before[name].ModTime(), info.ModTime(), "origin mtime must not move")
		require.Equal(before[name].Size(), info.Size(), "origin size must not move")
		got, err := os.ReadFile(filepath.Join(tp.BobSaveDir, name))
		require.NoError(err)
		require.Equal(content[name], got, "origin bytes must not move")
	}
}
