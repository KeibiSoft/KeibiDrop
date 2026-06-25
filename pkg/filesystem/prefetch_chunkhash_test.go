// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Verifies prefetchFile fills a per-chunk hash for every chunk it writes.
// ABOUTME: StreamFile sends 4 MiB blocks but the bitmap tracks 512 KiB chunks.

package filesystem

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
)

// blockStreamProvider serves content in config.BlockSize units, mirroring the
// real StreamFile server: one Recv yields a 4 MiB block (or the final partial
// block) at a block-aligned offset, so a single block spans several 512 KiB
// bitmap chunks. This is the condition Finding 3 fixes.
type blockStreamProvider struct{ content []byte }

func (p *blockStreamProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &slowStream{content: p.content}, nil
}

func (p *blockStreamProvider) StreamFile(_ context.Context, _ string, startOffset uint64) (types.StreamFileReceiver, error) {
	return &blockStreamReceiver{content: p.content, offset: startOffset}, nil
}

type blockStreamReceiver struct {
	content []byte
	offset  uint64
}

func (r *blockStreamReceiver) Recv() (data []byte, offset uint64, totalSize uint64, err error) {
	if r.offset >= uint64(len(r.content)) {
		return nil, 0, 0, io.EOF
	}
	end := r.offset + uint64(config.BlockSize)
	if end > uint64(len(r.content)) {
		end = uint64(len(r.content))
	}
	chunk := r.content[r.offset:end]
	cur := r.offset
	r.offset = end
	return chunk, cur, uint64(len(r.content)), nil
}

// TestPrefetchFile_FillsChunkHashes drives the real prefetchFile via startPrefetch
// over a multi-block file with a partial final chunk. Every chunk it writes must
// carry a hash matching xxh3.Hash of that chunk's exact bytes, not just the first
// chunk of each 4 MiB block. Pre-fix, prefetchFile Set only the head chunk of each
// block and stored no hashes, so a later SetHash could leave a prefetch-only chunk
// reporting Hash == (0, true) which reconcile would mistake for a real hash of 0.
func TestPrefetchFile_FillsChunkHashes(t *testing.T) {
	saveDir := t.TempDir()

	// 2 full 4 MiB blocks + 100 KiB: the final block is partial and its last
	// chunk is partial too, so we exercise interior and partial-span hashing.
	const extra = 100 * 1024
	fileSize := int64(2*config.BlockSize + extra)
	content := makePattern(int(fileSize))

	root := &Dir{
		RelativePath:        "/",
		RealPathOfFile:      saveDir,
		LocalDownloadFolder: saveDir,
		IsLocalPresent:      true,
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
		AllDirMap:           make(map[string]*Dir),
		AllFileMap:          make(map[string]*File),
		RemoteFiles:         make(map[string]*File),
		OpenStreamProvider:  func() types.FileStreamProvider { return &blockStreamProvider{content: content} },
		FsCtx:               context.Background(),
		PrefetchSem:         make(chan struct{}, 8),
		OpenMapLock:         sync.RWMutex{},
		Adm:                 sync.RWMutex{},
		AfmLock:             sync.RWMutex{},
		RemoteFilesLock:     sync.RWMutex{},
	}
	root.Root = root
	root.logger = nopLogger()

	path := "/big.bin"
	realPath := filepath.Clean(filepath.Join(saveDir, path))
	stat := &winfuse.Stat_t{Size: fileSize, Mode: 0o644 | winfuse.S_IFREG}
	if err := root.AddRemoteFile(root.logger, path, "big.bin", stat); err != nil {
		t.Fatalf("AddRemoteFile failed: %v", err)
	}

	root.RemoteFilesLock.RLock()
	f := root.RemoteFiles[path]
	root.RemoteFilesLock.RUnlock()
	if f == nil || f.Bitmap == nil {
		t.Fatal("remote file or bitmap not set up after AddRemoteFile")
	}

	root.startPrefetch(root.logger, f, path)

	waitFor(t, 5*time.Second, func() bool { return f.Bitmap.IsComplete() })

	cs := int64(ChunkSize)
	total := f.Bitmap.Total()

	// Every chunk must be present AND carry the correct hash of its exact slice.
	for c := 0; c < total; c++ {
		begin := int64(c) * cs
		end := begin + cs
		if end > fileSize {
			end = fileSize
		}
		if !f.Bitmap.Has(c) {
			t.Fatalf("chunk %d should be present after prefetch", c)
		}
		got, ok := f.Bitmap.Hash(c)
		if !ok {
			t.Fatalf("chunk %d hash not set after prefetch (Set without SetHash)", c)
		}
		want := xxh3.Hash(content[begin:end])
		if got != want {
			t.Fatalf("chunk %d hash mismatch: got %x, want %x", c, got, want)
		}
	}

	// The downloaded file content must be correct on disk.
	disk, err := os.ReadFile(realPath)
	if err != nil {
		t.Fatalf("read downloaded file: %v", err)
	}
	if len(disk) != len(content) {
		t.Fatalf("downloaded size %d, want %d", len(disk), len(content))
	}
}
