// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Stream pool for parallel gRPC reads on a single file.
// ABOUTME: Eliminates mutex contention by sharding FUSE readahead across N streams.

package filesystem

import (
	"context"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// StreamPool holds N parallel gRPC streams for a single file.
// FUSE reads pick a stream by chunk-index modulo N. This removes lock
// contention between concurrent readahead requests.
type StreamPool struct {
	streams []types.RemoteFileStream
	size    int
}

// NewStreamPool opens n parallel gRPC streams for the given file.
// On partial failure, it closes the already-open streams.
func NewStreamPool(provider types.FileStreamProvider, ctx context.Context, inode uint64, path string, n int) (*StreamPool, error) {
	streams := make([]types.RemoteFileStream, n)
	for i := range streams {
		s, err := provider.OpenRemoteFile(ctx, inode, path)
		if err != nil {
			for j := 0; j < i; j++ {
				_ = streams[j].Close()
			}
			return nil, err
		}
		streams[i] = s
	}
	return &StreamPool{streams: streams, size: n}, nil
}

// ReadAt routes the request to a stream selected by read-ahead block index.
// Consecutive 16 MiB blocks land on different streams, so a multi-block
// read-ahead window pipelines across the pool in parallel. Sharding by chunk
// index would map every block-aligned fetch (the only kind the on-demand path
// issues) to stream 0 and serialize the whole window on one stream. Any
// stream can serve any offset, so this is pure load distribution. Singleflight
// above this layer already collapses concurrent reads of the same block, so
// one stream per block adds no contention.
func (p *StreamPool) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	idx := int(offset/ReadAheadBlock) % p.size
	return p.streams[idx].ReadAt(ctx, offset, size)
}

// Close closes all streams in the pool.
func (p *StreamPool) Close() error {
	var firstErr error
	for _, s := range p.streams {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
