package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
)

// pullStreamFile downloads using a single server-streaming StreamFile RPC.
// No per-chunk round-trip. Best for WAN / relay connections.
func (kd *KeibiDrop) pullStreamFile(
	ctx context.Context,
	bitmap *filesystem.ChunkBitmap,
	f *os.File,
	relPath string,
	fileSize uint64,
	blockSize int,
	bitmapPath string,
	logger *slog.Logger,
) error {
	// Find first missing chunk to resume from.
	startOffset := uint64(0)
	for i := 0; i < bitmap.Total(); i++ {
		if !bitmap.Has(i) {
			startOffset = uint64(i) * uint64(blockSize)
			break
		}
	}

	stream, err := kd.session.GRPCClient.StreamFile(ctx, &bindings.StreamFileRequest{
		Path:        relPath,
		StartOffset: startOffset,
	})
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	chunksWritten := 0
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			logger.Error("Download failed (partial state preserved)", "error", err, "progress", bitmap.Progress())
			_ = bitmap.Save(bitmapPath)
			return fmt.Errorf("recv chunk: %w", err)
		}

		if _, err := f.WriteAt(resp.Data, int64(resp.Offset)); err != nil {
			_ = bitmap.Save(bitmapPath)
			return fmt.Errorf("write at offset %d: %w", resp.Offset, err)
		}

		chunkIdx := int(resp.Offset / uint64(blockSize))
		if chunkIdx < bitmap.Total() {
			bitmap.Set(chunkIdx)
		}

		chunksWritten++
		if chunksWritten%25 == 0 {
			_ = bitmap.Save(bitmapPath)
		}
	}
	return nil
}

// pullParallelRead downloads using N parallel bidirectional Read streams.
// Each worker sends ReadRequest per chunk and waits for ReadResponse.
// Best for LAN / loopback where RTT is negligible and parallelism helps.
func (kd *KeibiDrop) pullParallelRead(
	ctx context.Context,
	cancel context.CancelFunc,
	bitmap *filesystem.ChunkBitmap,
	f *os.File,
	relPath string,
	fileSize uint64,
	blockSize int,
	bitmapPath string,
	logger *slog.Logger,
) error {
	totalChunks := bitmap.Total()
	nWorkers := filesystem.StreamPoolSize
	if totalChunks < nWorkers {
		nWorkers = totalChunks
	}

	var wg sync.WaitGroup
	errCh := make(chan error, nWorkers)
	var chunksWritten atomic.Int32

	for w := 0; w < nWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			stream, err := kd.session.GRPCClient.Read(ctx)
			if err != nil {
				errCh <- fmt.Errorf("worker %d: open stream: %w", workerID, err)
				cancel()
				return
			}
			defer stream.CloseSend()

			for i := workerID; i < totalChunks; i += nWorkers {
				if bitmap.Has(i) {
					continue
				}

				offset := uint64(i) * uint64(blockSize)
				size := uint64(blockSize)
				if offset+size > fileSize {
					size = fileSize - offset
				}

				if err := stream.Send(&bindings.ReadRequest{
					Handle: 0,
					Path:   relPath,
					Offset: offset,
					Size:   uint32(size),
				}); err != nil {
					errCh <- fmt.Errorf("worker %d: send chunk %d: %w", workerID, i, err)
					cancel()
					return
				}

				data, err := stream.Recv()
				if err != nil {
					errCh <- fmt.Errorf("worker %d: recv chunk %d: %w", workerID, i, err)
					cancel()
					return
				}

				if uint64(len(data.Data)) != size {
					errCh <- fmt.Errorf("worker %d: chunk %d: got %d bytes, expected %d", workerID, i, len(data.Data), size)
					cancel()
					return
				}

				if _, err := f.WriteAt(data.Data, int64(offset)); err != nil {
					errCh <- fmt.Errorf("worker %d: write chunk %d: %w", workerID, i, err)
					cancel()
					return
				}

				bitmap.Set(i)

				if chunksWritten.Add(1)%25 == 0 {
					_ = bitmap.Save(bitmapPath)
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		logger.Error("Download failed (partial state preserved)", "error", err, "progress", bitmap.Progress())
		_ = bitmap.Save(bitmapPath)
		return err
	}
	return nil
}
