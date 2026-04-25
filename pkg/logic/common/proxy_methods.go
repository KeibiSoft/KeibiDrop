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
)

// Proxy methods to avoid import cycles between the filesystem and the duplex server.

type ImplFileStreamProvider struct {
	cli bindings.KeibiServiceClient
}

func NewImplStreamProvider(cli bindings.KeibiServiceClient) *ImplFileStreamProvider {
	return &ImplFileStreamProvider{cli: cli}
}

type ImplRemoteFileStream struct {
	stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse]
	handle uint64
	path   string
	mu     sync.Mutex // serializes Send+Recv pairs (gRPC streams are not concurrency-safe)
}

func NewImplRemoteFileStream(stream grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], inode uint64, path string) *ImplRemoteFileStream {
	return &ImplRemoteFileStream{
		stream: stream,
		handle: inode,
		path:   path,
	}
}
func (rfs *ImplRemoteFileStream) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	// Serialize Send+Recv: gRPC bidirectional streams are not safe for
	// concurrent Send() or concurrent Recv() from multiple goroutines.
	// FUSE issues parallel reads (readahead), so without this lock the
	// stream framing gets corrupted → ResourceExhausted with garbage sizes.
	rfs.mu.Lock()
	defer rfs.mu.Unlock()

	err := rfs.stream.Send(&bindings.ReadRequest{Handle: rfs.handle, Path: rfs.path, Offset: uint64(offset), Size: uint32(size)})
	if err != nil {
		return nil, err
	}

	resp, err := rfs.stream.Recv()
	if err != nil {
		return nil, err
	}

	return resp.Data, nil
}

func (rfs *ImplRemoteFileStream) Close() error {
	return rfs.stream.CloseSend()
}

func (sp *ImplFileStreamProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	stream, err := sp.cli.Read(ctx)
	if err != nil {
		return nil, err
	}

	return NewImplRemoteFileStream(stream, inode, path), nil
}

// StreamFile starts a push-based download using the server-streaming StreamFile RPC.
func (sp *ImplFileStreamProvider) StreamFile(ctx context.Context, path string, startOffset uint64) (types.StreamFileReceiver, error) {
	stream, err := sp.cli.StreamFile(ctx, &bindings.StreamFileRequest{
		Path:        path,
		StartOffset: startOffset,
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
