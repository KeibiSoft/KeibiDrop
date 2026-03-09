// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// Concurrency stress tests for Dir shared state — designed to be run with -race.

package filesystem

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// stubStreamProvider is a no-op FileStreamProvider for stress tests.
// OpenRemoteFile always returns an error so the prefetch goroutine exits
// immediately without blocking the test.
type stubStreamProvider struct{}

func (s *stubStreamProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return nil, errors.New("stub: no remote connection")
}

// newStressDir constructs a minimal Dir suitable for concurrency stress tests.
// It mirrors the construction in TestPrefetchRenameRace — no FUSE mount required.
func newStressDir(t *testing.T) *Dir {
	t.Helper()
	saveDir := t.TempDir()
	d := &Dir{
		Inode:               0,
		Name:                "",
		RelativePath:        "/",
		RealPathOfFile:      saveDir,
		LocalDownloadFolder: saveDir,
		IsLocalPresent:      true,
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*File),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*Dir),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*File),
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*File),
		// OpenStreamProvider returns a stub that immediately errors so the
		// background prefetch goroutine exits without blocking the test.
		OpenStreamProvider: func() types.FileStreamProvider {
			return &stubStreamProvider{}
		},
	}
	d.Root = d
	d.logger = nopLogger()
	return d
}

// TestDir_ConcurrentRemoteFilesAccess spawns N goroutines that simultaneously
// call AddRemoteFile() with unique paths and read RemoteFiles under RLock.
// The race detector must report no data races.
//
// Lock order verified: RemoteFilesLock → Adm → AfmLock (see fuse_directory.go Getattr).
func TestDir_ConcurrentRemoteFilesAccess(t *testing.T) {
	const N = 50
	d := newStressDir(t)

	stat := &winfuse.Stat_t{
		Size: 128,
		Mode: 0o644 | winfuse.S_IFREG,
	}

	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()

			path := fmt.Sprintf("/file-%04d.txt", i)
			name := fmt.Sprintf("file-%04d.txt", i)

			// Writer: AddRemoteFile acquires RemoteFilesLock internally.
			if err := d.AddRemoteFile(d.logger, path, name, stat); err != nil {
				// Not a test failure — the file may already exist if names collide,
				// but with unique indices that cannot happen here.
				t.Errorf("AddRemoteFile(%q) error: %v", path, err)
				return
			}

			// Reader: explicitly take RLock to read the map concurrently with writers.
			d.RemoteFilesLock.RLock()
			_, _ = d.RemoteFiles[path]
			count := len(d.RemoteFiles)
			d.RemoteFilesLock.RUnlock()

			// count must be at least 1 (the file we just added).
			if count < 1 {
				t.Errorf("expected RemoteFiles len >= 1 after insert, got %d", count)
			}
		}()
	}

	wg.Wait()

	// After all goroutines finish, every file must be present.
	d.RemoteFilesLock.RLock()
	defer d.RemoteFilesLock.RUnlock()
	if len(d.RemoteFiles) != N {
		t.Errorf("expected %d entries in RemoteFiles, got %d", N, len(d.RemoteFiles))
	}
}

// TestDir_ConcurrentOpenAndRelease spawns N goroutines that each insert a File
// into AllFileMap and OpenFileHandlers, then remove it — simulating the
// Open/Release lifecycle under concurrent load.
// The race detector must report no data races and there must be no deadlock.
func TestDir_ConcurrentOpenAndRelease(t *testing.T) {
	const N = 50
	d := newStressDir(t)

	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()

			fh := uint64(i + 1) // fd 0 is reserved; use 1-based handles.
			path := fmt.Sprintf("/open-%04d.bin", i)

			f := &File{
				Inode:        fh,
				Name:         fmt.Sprintf("open-%04d.bin", i),
				RelativePath: path,
				Parent:       d,
				Root:         d,
				logger:       nopLogger(),
			}

			// Simulate Open: add to AllFileMap and OpenFileHandlers.
			// Lock order: AfmLock first, then OpenMapLock (no Adm needed here).
			d.AfmLock.Lock()
			d.AllFileMap[path] = f
			d.AfmLock.Unlock()

			d.OpenMapLock.Lock()
			d.OpenFileHandlers[fh] = f
			d.OpenMapLock.Unlock()

			// Concurrent read of OpenFileHandlers (simulates another goroutine
			// checking if the handle is still open).
			d.OpenMapLock.RLock()
			_, _ = d.OpenFileHandlers[fh]
			d.OpenMapLock.RUnlock()

			// Simulate Release: remove from OpenFileHandlers and AllFileMap.
			d.OpenMapLock.Lock()
			delete(d.OpenFileHandlers, fh)
			d.OpenMapLock.Unlock()

			d.AfmLock.Lock()
			delete(d.AllFileMap, path)
			d.AfmLock.Unlock()
		}()
	}

	wg.Wait()

	// Both maps must be empty after all goroutines have completed.
	d.OpenMapLock.RLock()
	openLen := len(d.OpenFileHandlers)
	d.OpenMapLock.RUnlock()

	d.AfmLock.RLock()
	afmLen := len(d.AllFileMap)
	d.AfmLock.RUnlock()

	if openLen != 0 {
		t.Errorf("expected empty OpenFileHandlers after all releases, got %d entries", openLen)
	}
	if afmLen != 0 {
		t.Errorf("expected empty AllFileMap after all releases, got %d entries", afmLen)
	}
}

// TestDir_DownloadState_ConcurrentProgress spawns N goroutines that each call
// UpdateProgress() concurrently on a shared DownloadState, then verifies the
// final counters are consistent with no races or panics.
func TestDir_DownloadState_ConcurrentProgress(t *testing.T) {
	const N = 50
	const chunkSize = 1024 // 1 KiB per goroutine.
	const totalSize = uint64(N * chunkSize)

	var ds DownloadState
	ds.Reset(totalSize)

	var wg sync.WaitGroup
	wg.Add(N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			offset := int64(i * chunkSize)
			ds.UpdateProgress(offset, chunkSize)
		}()
	}

	wg.Wait()

	// BytesDownloaded is an atomic sum — all N goroutines each added chunkSize.
	got := ds.BytesDownloaded.Load()
	if got != totalSize {
		t.Errorf("BytesDownloaded = %d, want %d", got, totalSize)
	}

	// Progress must report 100 % because BytesDownloaded == TotalSize.
	if !ds.IsComplete() {
		t.Errorf("expected IsComplete() true after all chunks downloaded, progress=%.2f%%", ds.Progress())
	}
}
