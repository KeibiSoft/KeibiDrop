// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Stream pool for parallel gRPC reads on a single file.
// ABOUTME: Shards blocking misses across N streams; the read-ahead window gets its own lane.

package filesystem

import (
	"context"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// StreamPool holds N parallel gRPC streams for a single file, and one more on
// the provider's bulk lane, opened on the first read-ahead fetch. FUSE reads
// pick a stream by block index. This removes lock contention between
// concurrent readahead requests.
type StreamPool struct {
	streams []types.RemoteFileStream
	size    int

	provider types.FileStreamProvider
	ctx      context.Context
	inode    uint64
	path     string
	mu       sync.Mutex
	bulk     types.RemoteFileStream // The window lane; nil until the window fetches.
	closed   bool
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
	return &StreamPool{streams: streams, size: n, provider: provider, ctx: ctx, inode: inode, path: path}, nil
}

// ReadAt routes the request to a stream selected by read-ahead block index.
// Consecutive 16 MiB blocks land on different streams, so parallel misses
// pipeline across the pool. Sharding by chunk index would map every
// block-aligned fetch (the only kind the on-demand path issues) to stream 0
// and serialize them on one stream. Any stream can serve any offset, so this
// is pure load distribution. Singleflight above this layer already collapses
// concurrent reads of the same unit, so one stream per block adds no
// contention.
func (p *StreamPool) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	idx := int(offset/ReadAheadBlock) % p.size
	return p.streams[idx].ReadAt(ctx, offset, size)
}

// ReadAtBulk serves a predicted fetch, the read-ahead window, on the
// provider's bulk lane: a miss the reader waits on never queues behind a
// 16 MiB window fetch on one stream, and on a split session the QUIC lane
// stays with the miss while the window rides TCP (BUGS 28). A provider
// without the lane, or an open that fails, serves the fetch from the pool.
func (p *StreamPool) ReadAtBulk(ctx context.Context, offset int64, size int64) ([]byte, error) {
	if s := p.bulkStream(); s != nil {
		return s.ReadAt(ctx, offset, size)
	}
	return p.ReadAt(ctx, offset, size)
}

// bulkStream returns the window lane, opening it on first use. It is opened
// late because most opens never stream far enough for the window to fire.
func (p *StreamPool) bulkStream() types.RemoteFileStream {
	opener, ok := p.provider.(types.BulkReadOpener)
	if !ok {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.bulk == nil {
		s, err := opener.OpenRemoteFileBulk(p.ctx, p.inode, p.path)
		if err != nil {
			return nil // This fetch takes the pool; the next one tries again.
		}
		p.bulk = s
	}
	return p.bulk
}

// Close closes all streams in the pool.
func (p *StreamPool) Close() error {
	p.mu.Lock()
	p.closed = true
	bulk := p.bulk
	p.bulk = nil
	p.mu.Unlock()
	var firstErr error
	if bulk != nil {
		firstErr = bulk.Close()
	}
	for _, s := range p.streams {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
