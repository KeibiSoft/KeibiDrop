// ABOUTME: Unit tests for path traversal protection in MkdirFromPeer (KD-2026-001).
// ABOUTME: Verifies secureJoin blocks directory escape attempts from malicious peers.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// newTestDir builds a minimal Dir rooted at saveDir suitable for unit tests.
func newTestDir(saveDir string) *Dir {
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
	}
	d.Root = d
	d.logger = nopLogger()
	return d
}

// TestMkdirFromPeer_TraversalBlocked verifies that a peer-supplied path containing
// ".." components cannot escape LocalDownloadFolder (KD-2026-001).
func TestMkdirFromPeer_TraversalBlocked(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)

	errCode := d.MkdirFromPeer("../../escape", 0o755)

	if errCode == 0 {
		t.Errorf("expected non-zero errCode for traversal path, got %d", errCode)
	}

	escapedPath := filepath.Join(saveDir, "..", "..", "escape")
	if _, err := os.Stat(escapedPath); err == nil {
		t.Errorf("traversal directory must not exist at %q", escapedPath)
	}
}

// TestMkdirFromPeer_ValidPath verifies that a legitimate peer-supplied sub-directory
// path is created correctly inside LocalDownloadFolder.
func TestMkdirFromPeer_ValidPath(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)

	errCode := d.MkdirFromPeer("subdir", 0o755)

	if errCode != 0 {
		t.Errorf("expected errCode 0 for valid path, got %d", errCode)
	}

	expectedPath := filepath.Join(saveDir, "subdir")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected directory to exist at %q: %v", expectedPath, err)
	}
}
