// ABOUTME: Stream pool for parallel gRPC reads on a single file.
// ABOUTME: Eliminates mutex contention by sharding FUSE readahead across N streams.

package filesystem

import (
	"context"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// StreamPool holds N parallel gRPC streams for a single file.
// FUSE reads pick a stream by chunk-index modulo N, eliminating
// lock contention between concurrent readahead requests.
type StreamPool struct {
	streams []types.RemoteFileStream
	size    int
}

// NewStreamPool opens n parallel gRPC streams for the given file.
// On partial failure, already-opened streams are closed.
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

// ReadAt routes the request to a stream selected by chunk index.
// Different chunks hit different streams for true parallelism.
func (p *StreamPool) ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error) {
	idx := int(offset/ChunkSize) % p.size
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
