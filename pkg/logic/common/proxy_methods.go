// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"io"
	"sync"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mapReadErr maps a gRPC read error to a typed sentinel, so the filesystem layer
// does not import gRPC. NotFound becomes ErrRemoteFileNotFound, which surfaces as EOF.
func mapReadErr(err error) error {
	if status.Code(err) == codes.NotFound {
		return types.ErrRemoteFileNotFound
	}
	return err
}

// Proxy methods prevent import cycles between the filesystem and the duplex server.

type ImplFileStreamProvider struct {
	cli  bindings.KeibiServiceClient // Bulk channel (TCP): StreamFile prefetch.
	fast bindings.KeibiServiceClient // Interactive channel (QUIC): on-demand reads and chunk hashes. Nil = use cli.
}

func NewImplStreamProvider(cli bindings.KeibiServiceClient) *ImplFileStreamProvider {
	return &ImplFileStreamProvider{cli: cli}
}

// NewImplStreamProviderDual routes bulk prefetch to TCP, on-demand reads and chunk
// hashes to QUIC. A cache miss then does not queue behind a running prefetch.
func NewImplStreamProviderDual(bulk, fast bindings.KeibiServiceClient) *ImplFileStreamProvider {
	return &ImplFileStreamProvider{cli: bulk, fast: fast}
}

type ImplRemoteFileStream struct {
	stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse]
	handle uint64
	path   string
	mu     sync.Mutex // Serializes Send+Recv pairs. gRPC streams are not concurrency-safe.
}

func NewImplRemoteFileStream(stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], inode uint64, path string) *ImplRemoteFileStream {
	return &ImplRemoteFileStream{
		stream: stream,
		handle: inode,
		path:   path,
	}
}
func (rfs *ImplRemoteFileStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	// gRPC streams forbid concurrent Send/Recv. FUSE issues parallel readahead reads.
	// Without this lock the stream framing corrupts.
	rfs.mu.Lock()
	defer rfs.mu.Unlock()

	err := rfs.stream.Send(&bindings.ReadRequest{Handle: rfs.handle, Path: rfs.path, Offset: uint64(offset), Size: uint32(size)})
	if err != nil {
		// A Send error, commonly io.EOF, means the server closed the stream after a status.
		// Recv surfaces the real status, for example NotFound after a peer delete.
		if _, rerr := rfs.stream.Recv(); rerr != nil {
			return nil, mapReadErr(rerr)
		}
		return nil, mapReadErr(err)
	}

	resp, err := rfs.stream.Recv()
	if err != nil {
		return nil, mapReadErr(err)
	}

	return resp.Data, nil
}

func (rfs *ImplRemoteFileStream) Close() error {
	return rfs.stream.CloseSend()
}

// tryThen opens on primary, then on secondary after an error. It skips nil clients
// and surfaces the meaningful error.
func tryThen[T any](primary, secondary bindings.KeibiServiceClient, open func(bindings.KeibiServiceClient) (T, error)) (T, error) {
	var firstErr error
	if primary != nil {
		v, err := open(primary)
		if err == nil {
			return v, nil
		}
		firstErr = err
	}
	if secondary != nil {
		return open(secondary)
	}
	if firstErr == nil {
		firstErr = errors.New("stream provider: no client available")
	}
	var zero T
	return zero, firstErr
}

// preferFast routes latency-sensitive opens: the QUIC client first, then the bulk
// client. A dead QUIC channel degrades to bulk.
func preferFast[T any](sp *ImplFileStreamProvider, open func(bindings.KeibiServiceClient) (T, error)) (T, error) {
	return tryThen(sp.fast, sp.cli, open)
}

// preferBulk routes bulk transfers: TCP first, because TCP measures faster for bulk.
// When TCP dies mid-session, transfers continue over QUIC until reconnect rebuilds TCP.
func preferBulk[T any](sp *ImplFileStreamProvider, open func(bindings.KeibiServiceClient) (T, error)) (T, error) {
	return tryThen(sp.cli, sp.fast, open)
}

func (sp *ImplFileStreamProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	stream, err := preferFast(sp, func(c bindings.KeibiServiceClient) (grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], error) {
		return c.Read(ctx)
	})
	if err != nil {
		return nil, err
	}

	return NewImplRemoteFileStream(stream, inode, path), nil
}

// StreamFile starts a push-based download via the server-streaming StreamFile RPC.
// It uses preferBulk routing: TCP first, QUIC when TCP is dead.
func (sp *ImplFileStreamProvider) StreamFile(ctx context.Context, path string, startOffset uint64) (types.StreamFileReceiver, error) {
	stream, err := preferBulk(sp, func(c bindings.KeibiServiceClient) (bindings.KeibiService_StreamFileClient, error) {
		return c.StreamFile(ctx, &bindings.StreamFileRequest{
			Path:        path,
			StartOffset: startOffset,
		})
	})
	if err != nil {
		return nil, err
	}
	return &implStreamFileReceiver{stream: stream}, nil
}

type implStreamFileReceiver struct {
	stream bindings.KeibiService_StreamFileClient
}

func (r *implStreamFileReceiver) Recv() (data []byte, offset uint64, totalSize uint64, err error) {
	resp, err := r.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, 0, 0, io.EOF
		}
		return nil, 0, 0, err
	}
	return resp.Data, resp.Offset, resp.TotalSize, nil
}

// mapBatchErr maps Unimplemented to the typed sentinel, so the filesystem layer
// can disable warming for the session without importing gRPC.
func mapBatchErr(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return types.ErrBatchUnsupported
	}
	return err
}

// ReadBatch starts a batched small-file download via the server-streaming
// ReadBatch RPC. It uses preferBulk routing so warming never queues ahead of
// latency-sensitive on-demand reads. An older peer surfaces
// ErrBatchUnsupported at the first Recv, not at open.
func (sp *ImplFileStreamProvider) ReadBatch(ctx context.Context, paths []string, maxBytes uint64) (types.BatchFrameReceiver, error) {
	stream, err := preferBulk(sp, func(c bindings.KeibiServiceClient) (bindings.KeibiService_ReadBatchClient, error) {
		return c.ReadBatch(ctx, &bindings.ReadBatchRequest{Paths: paths, MaxBytes: maxBytes})
	})
	if err != nil {
		return nil, mapBatchErr(err)
	}
	return &implBatchFrameReceiver{stream: stream}, nil
}

type implBatchFrameReceiver struct {
	stream bindings.KeibiService_ReadBatchClient
}

func (r *implBatchFrameReceiver) Recv() (types.BatchFrame, error) {
	resp, err := r.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return types.BatchFrame{}, io.EOF
		}
		return types.BatchFrame{}, mapBatchErr(err)
	}
	return types.BatchFrame{
		Path:      resp.Path,
		Offset:    resp.Offset,
		Data:      resp.Data,
		TotalSize: resp.TotalSize,
		FileDone:  resp.FileDone,
		ErrCode:   resp.ErrCode,
	}, nil
}

// GetChunkHashes requests per-chunk xxh3-64 fingerprints from the peer. An older
// peer returns codes.Unimplemented. The caller decides the fallback.
func (sp *ImplFileStreamProvider) GetChunkHashes(ctx context.Context, path string, chunkSize, fromChunk, count uint64) (types.ChunkHashReceiver, error) {
	req := &bindings.GetChunkHashesRequest{
		Path:      path,
		ChunkSize: chunkSize,
		FromChunk: fromChunk,
		Count:     count,
	}
	// Chunk hashes are small and latency-sensitive. They use the same preferFast
	// routing as on-demand reads.
	stream, err := preferFast(sp, func(c bindings.KeibiServiceClient) (bindings.KeibiService_GetChunkHashesClient, error) {
		return c.GetChunkHashes(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return &implChunkHashReceiver{stream: stream}, nil
}

type implChunkHashReceiver struct {
	stream bindings.KeibiService_GetChunkHashesClient
}

func (r *implChunkHashReceiver) Recv() (chunkIndex uint64, hash uint64, err error) {
	resp, err := r.stream.Recv()
	if err != nil {
		return 0, 0, err
	}
	return resp.ChunkIndex, resp.Hash, nil
}

// Compile-time assertion: ImplFileStreamProvider implements ChunkHasher.
var _ types.ChunkHasher = (*ImplFileStreamProvider)(nil)

// Compile-time assertion: ImplFileStreamProvider implements BatchReader.
var _ types.BatchReader = (*ImplFileStreamProvider)(nil)
