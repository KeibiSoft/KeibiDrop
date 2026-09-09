// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file pins the two read lanes of a stream pool: a miss the reader waits
// on fetches through the pool, the read-ahead window through the bulk lane.

package filesystem

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// laneProvider serves the same content on two lanes and counts each, so a test
// can tell which lane a fetch took: miss is the pool's lane, window the bulk one.
type laneProvider struct {
	miss   *countingProvider
	window *countingProvider
}

func (p *laneProvider) OpenRemoteFile(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	return p.miss.OpenRemoteFile(ctx, inode, path)
}

func (p *laneProvider) OpenRemoteFileBulk(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	return p.window.OpenRemoteFile(ctx, inode, path)
}

func (p *laneProvider) StreamFile(ctx context.Context, path string, startOffset uint64) (types.StreamFileReceiver, error) {
	return p.miss.StreamFile(ctx, path, startOffset)
}

// A sequential scan: the units the reader waits on take the pool, every window
// block takes the bulk lane, and no byte crosses twice.
func TestStreamPool_WindowFetchesRideTheBulkLane(t *testing.T) {
	fileSize := 4 * ReadAheadBlock
	content := makePattern(fileSize)
	prov := &laneProvider{miss: &countingProvider{content: content}, window: &countingProvider{content: content}}
	root, fh, cleanup := newReadAheadFileWith(t, int64(fileSize), prov)
	defer cleanup()
	root.ReadAheadWindowBlocks = 2
	f := root.OpenFileHandlers[fh].File

	got := readSeq(root, fh, fileSize, 128)
	if !bytes.Equal(got, content) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), fileSize)
	}
	waitFor(t, 3*time.Second, func() bool { return inflightEmpty(f) })

	missBytes, windowBytes := prov.miss.bytesIn.Load(), prov.window.bytesIn.Load()
	if windowBytes == 0 {
		t.Fatalf("the read-ahead window fetched nothing on the bulk lane (pool fetches %d)", prov.miss.reads.Load())
	}
	if missBytes == 0 {
		t.Fatalf("no blocking miss took the pool lane (window fetches %d)", prov.window.reads.Load())
	}
	if missBytes+windowBytes != int64(fileSize) {
		t.Fatalf("bytes fetched: pool %d + window %d, want %d (every byte once across both lanes)", missBytes, windowBytes, fileSize)
	}
	if c, w := root.raPrefetchCalls.Load(), prov.window.reads.Load(); c != w {
		t.Fatalf("window blocks fetched %d, bulk-lane fetches %d: the window and nothing else takes the bulk lane", c, w)
	}
}

// A provider without the bulk lane serves the window from the pool, so older
// providers and the counting fake keep one fetch per block.
func TestStreamPool_ReadAtBulkFallsBackToThePool(t *testing.T) {
	content := makePattern(2 * ChunkSize)
	prov := &countingProvider{content: content}
	pool, err := NewStreamPool(prov, context.Background(), 1, "/f.bin", 2)
	if err != nil {
		t.Fatalf("new stream pool: %v", err)
	}
	defer func() { _ = pool.Close() }()

	data, err := pool.ReadAtBulk(context.Background(), 0, int64(ChunkSize))
	if err != nil || !bytes.Equal(data, content[:ChunkSize]) {
		t.Fatalf("ReadAtBulk without a bulk lane: err %v, %d bytes", err, len(data))
	}
	if reads := prov.reads.Load(); reads != 1 {
		t.Fatalf("pool fetches = %d, want 1", reads)
	}
}

// failingBulkProvider has a bulk lane that can be down, and counts its opens.
type failingBulkProvider struct {
	*countingProvider
	down  bool
	opens int
}

func (p *failingBulkProvider) OpenRemoteFileBulk(ctx context.Context, inode uint64, path string) (types.RemoteFileStream, error) {
	p.opens++
	if p.down {
		return nil, errors.New("bulk lane down")
	}
	return p.OpenRemoteFile(ctx, inode, path)
}

// A bulk lane that fails to open costs the window nothing: the fetch takes the
// pool, the next fetch tries the lane again, an open lane is reused, and a
// closed pool opens no lane.
func TestStreamPool_BulkLaneOpenFailureFallsBack(t *testing.T) {
	ctx := context.Background()
	content := makePattern(2 * ChunkSize)
	prov := &failingBulkProvider{countingProvider: &countingProvider{content: content}, down: true}
	pool, err := NewStreamPool(prov, ctx, 1, "/f.bin", 1)
	if err != nil {
		t.Fatalf("new stream pool: %v", err)
	}

	if _, err := pool.ReadAtBulk(ctx, 0, int64(ChunkSize)); err != nil {
		t.Fatalf("fetch with the bulk lane down: %v", err)
	}
	if prov.opens != 1 || prov.reads.Load() != 1 {
		t.Fatalf("lane down: opens %d reads %d, want 1 and 1 (served by the pool)", prov.opens, prov.reads.Load())
	}
	prov.down = false
	for i := 0; i < 2; i++ {
		if _, err := pool.ReadAtBulk(ctx, 0, int64(ChunkSize)); err != nil {
			t.Fatalf("fetch %d with the lane up: %v", i, err)
		}
	}
	if prov.opens != 2 {
		t.Fatalf("lane up: opens %d, want 2 (one retry, then reused)", prov.opens)
	}
	if err := pool.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if s := pool.bulkStream(); s != nil || prov.opens != 2 {
		t.Fatalf("closed pool opened a lane (stream %v, opens %d)", s != nil, prov.opens)
	}
}
