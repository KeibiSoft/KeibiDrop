// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
package common

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
)

// pullParallelRead downloads using N parallel bidirectional Read streams.
// Each worker owns a shard of chunk indices and processes them sequentially.
// 4 workers with interleaved shards saturate the link while keeping per-chunk
// ordering within each stream (required by gRPC).
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
					Size:   uint32(size), // #nosec G115 -- size is bounded by blockSize
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
