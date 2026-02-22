// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This file tests the prefetch-rename race: a file renamed while download is in flight.

package filesystem

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/assert"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// nopLogger returns a slog.Logger that discards all output, used in unit tests.
func nopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// slowStreamProvider simulates a slow download so the rename can race ahead.
// It serves a fixed content buffer, with an optional delay before each chunk.
type slowStreamProvider struct {
	content []byte
	delay   time.Duration
}

func (p *slowStreamProvider) OpenRemoteFile(_ context.Context, _ uint64, _ string) (types.RemoteFileStream, error) {
	return &slowStream{content: p.content, delay: p.delay}, nil
}

type slowStream struct {
	content []byte
	delay   time.Duration
}

func (s *slowStream) ReadAt(_ context.Context, offset int64, size int64) ([]byte, error) {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	end := offset + size
	if end > int64(len(s.content)) {
		end = int64(len(s.content))
	}
	out := make([]byte, end-offset)
	copy(out, s.content[offset:end])
	return out, nil
}

func (s *slowStream) Close() error { return nil }

// TestPrefetchRenameRace verifies that when a RENAME_FILE event arrives while a
// background prefetch goroutine is downloading a file, the final content ends up
// at the renamed path (not the original path), with correct non-zero bytes.
//
// This is the git lock-then-rename pattern: Alice writes HEAD.lock then renames
// to HEAD. Bob should end up with HEAD containing correct content, not zeros.
func TestPrefetchRenameRace(t *testing.T) {
	saveDir := t.TempDir()

	// Content to sync: simulates "ref: refs/heads/master\n" (23 bytes).
	content := []byte("ref: refs/heads/master\n")
	fileSize := int64(len(content))

	// slowStreamProvider adds a small delay per chunk so the rename can race.
	provider := &slowStreamProvider{content: content, delay: 20 * time.Millisecond}

	openStreamProvider := func() types.FileStreamProvider {
		return provider
	}

	root := &Dir{
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
		OpenStreamProvider:  openStreamProvider,
		PrefetchOnOpen:      true,
	}
	root.Root = root
	root.logger = nopLogger()

	lockPath := "/.git/HEAD.lock"
	finalPath := "/.git/HEAD"

	stat := &winfuse.Stat_t{
		Size: fileSize,
		Mode: 0o644 | winfuse.S_IFREG,
	}

	// AddRemoteFile starts the prefetch goroutine for HEAD.lock.
	if err := root.AddRemoteFile(root.logger, lockPath, "HEAD.lock", stat); err != nil {
		t.Fatalf("AddRemoteFile failed: %v", err)
	}

	// Immediately simulate the RENAME_FILE notification arriving on Bob's side,
	// before the prefetch goroutine has finished.
	root.RemoteFilesLock.Lock()
	file, exists := root.RemoteFiles[lockPath]
	if !exists {
		root.RemoteFilesLock.Unlock()
		t.Fatal("HEAD.lock not found in RemoteFiles after AddRemoteFile")
	}
	delete(root.RemoteFiles, lockPath)
	file.RelativePath = finalPath
	file.Name = "HEAD"
	// Update RealPathOfFile so prefetchFile can detect the rename.
	file.RealPathOfFile = filepath.Clean(filepath.Join(saveDir, finalPath))
	root.RemoteFiles[finalPath] = file
	root.RemoteFilesLock.Unlock()

	// Wait for the prefetch goroutine to complete (up to 5 seconds).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		root.RemoteFilesLock.RLock()
		f, ok := root.RemoteFiles[finalPath]
		root.RemoteFilesLock.RUnlock()
		if ok && f.Bitmap != nil && f.Bitmap.IsComplete() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Give a little extra time for the goroutine to flush and rename.
	time.Sleep(100 * time.Millisecond)

	finalDiskPath := filepath.Clean(filepath.Join(saveDir, finalPath))

	// The final file must exist with correct content.
	got, err := os.ReadFile(finalDiskPath)
	if err != nil {
		t.Fatalf("final file %q not found after prefetch+rename: %v", finalDiskPath, err)
	}
	if string(got) != string(content) {
		t.Fatalf("final file content wrong:\n  got:  %q\n  want: %q", got, content)
	}

	// The original lock file must NOT exist after the atomic os.Rename moved it
	// to the final path.
	lockDiskPath := filepath.Clean(filepath.Join(saveDir, lockPath))
	_, lockErr := os.Stat(lockDiskPath)
	assert.True(t, os.IsNotExist(lockErr), "lock file should not exist after successful rename: %v", lockErr)
}
