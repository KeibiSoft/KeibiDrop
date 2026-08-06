// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

// ABOUTME: Tests for GetChunkHashes handler: hash correctness, sub-range, path auth parity.
// ABOUTME: Verifies the new RPC shares StreamFile's path resolution (no new traversal surface).

package service

import (
	"context"
	"io"
	"log/slog"
	"math"
	"os"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockGetChunkHashesStream implements bindings.KeibiService_GetChunkHashesServer.
type mockGetChunkHashesStream struct {
	sent []*bindings.GetChunkHashesResponse
}

func (m *mockGetChunkHashesStream) Send(resp *bindings.GetChunkHashesResponse) error {
	m.sent = append(m.sent, resp)
	return nil
}

func (m *mockGetChunkHashesStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockGetChunkHashesStream) SendHeader(metadata.MD) error { return nil }
func (m *mockGetChunkHashesStream) SetTrailer(metadata.MD)       {}
func (m *mockGetChunkHashesStream) Context() context.Context     { return context.Background() }
func (m *mockGetChunkHashesStream) SendMsg(interface{}) error    { return nil }
func (m *mockGetChunkHashesStream) RecvMsg(interface{}) error    { return nil }

// newTestSvc builds a KeibidropServiceImpl backed by AllFileMap and LocalFiles.
func newTestSvc(t *testing.T, localPath string, trackerKey string) *KeibidropServiceImpl {
	t.Helper()
	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(&filesystem.Dir{
		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*filesystem.File),
	})
	svc.SetFS(fsForSvc)
	if trackerKey != "" && localPath != "" {
		svc.SyncTracker.LocalFiles[trackerKey] = &synctracker.File{
			Name:           trackerKey,
			RelativePath:   trackerKey,
			RealPathOfFile: localPath,
		}
	}
	return svc
}

// TestGetChunkHashes_HashCorrectness verifies chunk hashes match xxh3.Hash of
// each slice, including a final partial chunk smaller than chunk_size.
func TestGetChunkHashes_HashCorrectness(t *testing.T) {
	// 10 bytes, chunk_size=4: chunks 0=[0:4], 1=[4:8], 2=[8:10] (partial).
	content := []byte("0123456789")
	tmpFile, err := os.CreateTemp(t.TempDir(), "hashes-*.bin")
	require.NoError(t, err)
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := newTestSvc(t, tmpFile.Name(), "hashes.bin")

	stream := &mockGetChunkHashesStream{}
	req := &bindings.GetChunkHashesRequest{
		Path:      "hashes.bin",
		ChunkSize: 4,
		FromChunk: 0,
		Count:     0,
	}
	err = svc.GetChunkHashes(req, stream)
	require.NoError(t, err)
	require.Len(t, stream.sent, 3)

	assert.Equal(t, uint64(0), stream.sent[0].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[0:4]), stream.sent[0].Hash)

	assert.Equal(t, uint64(1), stream.sent[1].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[4:8]), stream.sent[1].Hash)

	assert.Equal(t, uint64(2), stream.sent[2].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[8:10]), stream.sent[2].Hash)
}

// TestGetChunkHashes_SubRange verifies from_chunk and count limit the response.
func TestGetChunkHashes_SubRange(t *testing.T) {
	// 12 bytes, chunk_size=4: chunks 0,1,2. Request only chunk 1 (count=1).
	content := []byte("abcdefghijkl")
	tmpFile, err := os.CreateTemp(t.TempDir(), "subrange-*.bin")
	require.NoError(t, err)
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := newTestSvc(t, tmpFile.Name(), "subrange.bin")

	stream := &mockGetChunkHashesStream{}
	req := &bindings.GetChunkHashesRequest{
		Path:      "subrange.bin",
		ChunkSize: 4,
		FromChunk: 1,
		Count:     1,
	}
	err = svc.GetChunkHashes(req, stream)
	require.NoError(t, err)
	require.Len(t, stream.sent, 1)

	assert.Equal(t, uint64(1), stream.sent[0].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[4:8]), stream.sent[0].Hash)
}

// TestGetChunkHashes_ZeroChunkSize verifies InvalidArgument is returned.
func TestGetChunkHashes_ZeroChunkSize(t *testing.T) {
	svc := newTestSvc(t, "", "")

	stream := &mockGetChunkHashesStream{}
	req := &bindings.GetChunkHashesRequest{
		Path:      "any.bin",
		ChunkSize: 0,
	}
	err := svc.GetChunkHashes(req, stream)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.InvalidArgument, st.Code())
}

// TestGetChunkHashes_OversizeChunkSize verifies a peer-supplied chunk_size larger
// than the streaming buffer is rejected with InvalidArgument BEFORE any allocation,
// so a malicious value (MaxUint64 or several GiB) cannot OOM/panic the process.
func TestGetChunkHashes_OversizeChunkSize(t *testing.T) {
	svc := newTestSvc(t, "", "")

	for _, cs := range []uint64{math.MaxUint64, uint64(config.GRPCStreamBuffer) + 1, 1 << 30} {
		stream := &mockGetChunkHashesStream{}
		req := &bindings.GetChunkHashesRequest{
			Path:      "any.bin",
			ChunkSize: cs,
		}
		// Must return InvalidArgument and not panic/allocate; the over-range check
		// runs before path resolution, so no real file is needed.
		err := svc.GetChunkHashes(req, stream)
		require.Error(t, err, "chunk_size=%d", cs)
		st, ok := status.FromError(err)
		require.True(t, ok, "chunk_size=%d", cs)
		assert.Equal(t, codes.InvalidArgument, st.Code(), "chunk_size=%d", cs)
		assert.Empty(t, stream.sent, "chunk_size=%d must send nothing", cs)
	}
}

// TestGetChunkHashes_PathNotFound_ParityWithStreamFile verifies that a path
// not present in AllFileMap or LocalFiles returns NotFound, which is the same
// status StreamFile returns for the same bad path. This confirms no new
// traversal surface: both RPCs share the same resolution logic.
func TestGetChunkHashes_PathNotFound_ParityWithStreamFile(t *testing.T) {
	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(&filesystem.Dir{
		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*filesystem.File),
	})
	svc.SetFS(fsForSvc)

	// GetChunkHashes on unknown path.
	chStream := &mockGetChunkHashesStream{}
	chErr := svc.GetChunkHashes(&bindings.GetChunkHashesRequest{
		Path:      "nonexistent.bin",
		ChunkSize: 512,
	}, chStream)
	require.Error(t, chErr)
	chSt, ok := status.FromError(chErr)
	require.True(t, ok)
	assert.Equal(t, codes.NotFound, chSt.Code())

	// StreamFile on the same unknown path must return the same status code.
	sfStream := &mockStreamFileStream{}
	sfErr := svc.StreamFile(&bindings.StreamFileRequest{Path: "nonexistent.bin"}, sfStream)
	require.Error(t, sfErr)
	sfSt, ok := status.FromError(sfErr)
	require.True(t, ok)
	assert.Equal(t, sfSt.Code(), chSt.Code(), "GetChunkHashes and StreamFile must return same status for bad path")
}

// TestGetChunkHashes_NoFUSE_HashCorrectness exercises the no-FUSE resolution
// branch (kd.FS == nil, SyncTracker.LocalFiles lookup) with a multi-chunk
// file including a partial final chunk.
func TestGetChunkHashes_NoFUSE_HashCorrectness(t *testing.T) {
	// 11 bytes, chunk_size=4: chunks 0=[0:4], 1=[4:8], 2=[8:11] (partial).
	content := []byte("HelloWorld!")
	tmpFile, err := os.CreateTemp(t.TempDir(), "nofuse-hashes-*.bin")
	require.NoError(t, err)
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	tmpFile.Close()

	svc := &KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	svc.SyncTracker.LocalFiles["nofuse.bin"] = &synctracker.File{
		Name:           "nofuse.bin",
		RelativePath:   "nofuse.bin",
		RealPathOfFile: tmpFile.Name(),
	}

	stream := &mockGetChunkHashesStream{}
	req := &bindings.GetChunkHashesRequest{
		Path:      "nofuse.bin",
		ChunkSize: 4,
		FromChunk: 0,
		Count:     0,
	}
	err = svc.GetChunkHashes(req, stream)
	require.NoError(t, err)
	require.Len(t, stream.sent, 3)

	assert.Equal(t, uint64(0), stream.sent[0].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[0:4]), stream.sent[0].Hash)

	assert.Equal(t, uint64(1), stream.sent[1].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[4:8]), stream.sent[1].Hash)

	assert.Equal(t, uint64(2), stream.sent[2].ChunkIndex)
	assert.Equal(t, xxh3.Hash(content[8:11]), stream.sent[2].Hash)
}

// mockStreamFileStream is the minimal stub needed for the parity test.
type mockStreamFileStream struct{}

func (m *mockStreamFileStream) Send(*bindings.StreamFileResponse) error { return nil }
func (m *mockStreamFileStream) SetHeader(metadata.MD) error             { return nil }
func (m *mockStreamFileStream) SendHeader(metadata.MD) error            { return nil }
func (m *mockStreamFileStream) SetTrailer(metadata.MD)                  {}
func (m *mockStreamFileStream) Context() context.Context                { return context.Background() }
func (m *mockStreamFileStream) SendMsg(interface{}) error               { return nil }
func (m *mockStreamFileStream) RecvMsg(interface{}) error               { return nil }
