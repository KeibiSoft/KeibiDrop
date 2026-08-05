// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

// ABOUTME: Integration tests for GetChunkHashes: real client-server round-trip over bufconn.
// ABOUTME: Tests A (hash correctness incl. partial final chunk) and B (Unimplemented fallback).

package common_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/service"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/xxh3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

const bufSize = 1 << 20 // 1 MiB

// startServer registers svc on a bufconn listener, starts serving in a
// goroutine, and returns a client connected to that listener plus a stop func.
func startServer(t *testing.T, svc bindings.KeibiServiceServer) (*grpc.ClientConn, func()) {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	bindings.RegisterKeibiServiceServer(srv, svc)
	go func() {
		if err := srv.Serve(lis); err != nil && err != grpc.ErrServerStopped {
			t.Errorf("srv.Serve: %v", err)
		}
	}()
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	}
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(dial),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	stop := func() {
		srv.Stop()
		_ = conn.Close()
		_ = lis.Close()
	}
	return conn, stop
}

// newTestSvcIntegration builds a KeibidropServiceImpl backed by SyncTracker.LocalFiles,
// matching the pattern used in service_chunkhashes_test.go.
func newTestSvcIntegration(t *testing.T, localPath, trackerKey string) *service.KeibidropServiceImpl {
	t.Helper()
	svc := &service.KeibidropServiceImpl{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	svc.SetFS(&filesystem.FS{
		Root: &filesystem.Dir{
			AfmLock:    sync.RWMutex{},
			AllFileMap: make(map[string]*filesystem.File),
		},
	})
	if trackerKey != "" && localPath != "" {
		svc.SyncTracker.LocalFiles[trackerKey] = &synctracker.File{
			Name:           trackerKey,
			RelativePath:   trackerKey,
			RealPathOfFile: localPath,
		}
	}
	return svc
}

// TestGetChunkHashes_RealRoundTrip_HashCorrectness verifies that the real
// ImplFileStreamProvider.GetChunkHashes method talks to the real server handler
// over a bufconn channel and returns the correct xxh3-64 hashes, including a
// partial final chunk when the file size is not a multiple of chunk_size.
// It also exercises a sub-range (fromChunk>0, count>0) round-trip.
func TestGetChunkHashes_RealRoundTrip_HashCorrectness(t *testing.T) {
	// 14 bytes, chunk_size=4: chunks [0:4], [4:8], [8:12], [12:14] (partial).
	content := []byte("ABCDEFGHIJKLMN")
	tmpFile, err := os.CreateTemp(t.TempDir(), "rt-hashes-*.bin")
	require.NoError(t, err)
	_, err = tmpFile.Write(content)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	svc := newTestSvcIntegration(t, tmpFile.Name(), "rt.bin")
	conn, stop := startServer(t, svc)
	defer stop()

	provider := common.NewImplStreamProvider(bindings.NewKeibiServiceClient(conn))

	ctx := context.Background()
	const chunkSize = 4

	// Full range (count=0 means to EOF).
	recv, err := provider.GetChunkHashes(ctx, "rt.bin", chunkSize, 0, 0)
	require.NoError(t, err)

	type chunkResult struct {
		index uint64
		hash  uint64
	}
	var results []chunkResult
	for {
		idx, hash, recvErr := recv.Recv()
		if recvErr == io.EOF {
			break
		}
		require.NoError(t, recvErr)
		results = append(results, chunkResult{idx, hash})
	}

	// 14 bytes / 4 bytes per chunk = 4 chunks (last one partial, 2 bytes).
	require.Len(t, results, 4, "expected 4 chunks for 14-byte file with chunk_size=4")

	assert.Equal(t, uint64(0), results[0].index)
	assert.Equal(t, xxh3.Hash(content[0:4]), results[0].hash)

	assert.Equal(t, uint64(1), results[1].index)
	assert.Equal(t, xxh3.Hash(content[4:8]), results[1].hash)

	assert.Equal(t, uint64(2), results[2].index)
	assert.Equal(t, xxh3.Hash(content[8:12]), results[2].hash)

	assert.Equal(t, uint64(3), results[3].index)
	assert.Equal(t, xxh3.Hash(content[12:14]), results[3].hash, "partial final chunk hash must match")

	// Sub-range: request only chunks 1 and 2 (fromChunk=1, count=2).
	recv2, err := provider.GetChunkHashes(ctx, "rt.bin", chunkSize, 1, 2)
	require.NoError(t, err)

	var subResults []chunkResult
	for {
		idx, hash, recvErr := recv2.Recv()
		if recvErr == io.EOF {
			break
		}
		require.NoError(t, recvErr)
		subResults = append(subResults, chunkResult{idx, hash})
	}

	require.Len(t, subResults, 2, "expected exactly 2 chunks for sub-range [1,3)")
	assert.Equal(t, uint64(1), subResults[0].index)
	assert.Equal(t, xxh3.Hash(content[4:8]), subResults[0].hash)
	assert.Equal(t, uint64(2), subResults[1].index)
	assert.Equal(t, xxh3.Hash(content[8:12]), subResults[1].hash)
}

// bareUnimplementedServer wraps UnimplementedKeibiServiceServer to provide a
// server that returns codes.Unimplemented for every RPC, simulating an older
// peer that does not have the GetChunkHashes method.
type bareUnimplementedServer struct {
	bindings.UnimplementedKeibiServiceServer
}

// TestGetChunkHashes_RealRoundTrip_UnimplementedFallback verifies that calling
// GetChunkHashes against a bare UnimplementedKeibiServiceServer returns
// codes.Unimplemented. This proves the backward-compat fallback path in the
// reconcile logic fires correctly for older peers.
func TestGetChunkHashes_RealRoundTrip_UnimplementedFallback(t *testing.T) {
	conn, stop := startServer(t, &bareUnimplementedServer{})
	defer stop()

	provider := common.NewImplStreamProvider(bindings.NewKeibiServiceClient(conn))

	ctx := context.Background()
	recv, err := provider.GetChunkHashes(ctx, "any.bin", 512, 0, 0)
	// The Unimplemented status may surface at call time or on first Recv.
	if err != nil {
		require.Equal(t, codes.Unimplemented, status.Code(err),
			"expected Unimplemented from bare server, got %v", err)
		return
	}
	// If GetChunkHashes itself succeeded (stream opened), the error arrives on Recv.
	require.NotNil(t, recv)
	_, _, recvErr := recv.Recv()
	require.Error(t, recvErr)
	assert.Equal(t, codes.Unimplemented, status.Code(recvErr),
		"expected Unimplemented on first Recv from bare server, got %v", recvErr)
}
