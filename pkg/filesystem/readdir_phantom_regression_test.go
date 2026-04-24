// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Regression test for issue #63 — phantom FUSE entries at wrong directory level.
// ABOUTME: Calls Readdir directly to verify no phantom basenames leak from nested remote paths.

package filesystem

import (
	"os"
	"sync"
	"testing"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
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

// fillCapture returns a fill func that appends emitted names into the provided slice.
func fillCapture(names *[]string) func(string, *winfuse.Stat_t, int64) bool {
	return func(name string, _ *winfuse.Stat_t, _ int64) bool {
		*names = append(*names, name)
		return true
	}
}

// TestReaddir_NoPhantomEntriesAtRoot is the direct regression for issue #63.
// Before the fix: listing "/" would emit "git.adoc" as a phantom direct child
// because the old loop extracted basename from every RemoteFiles key without
// path-level filtering.
func TestReaddir_NoPhantomEntriesAtRoot(t *testing.T) {
	localRoot := t.TempDir()
	d := newTestDir(localRoot)

	// Populate RemoteFiles exactly as in the bug report.
	d.RemoteFiles["/test_repo/Documentation/git.adoc"] = &File{}
	d.RemoteFiles["/test_repo/README.md"] = &File{}
	d.RemoteFiles["/photos/vacation/beach.jpg"] = &File{}
	d.RemoteFiles["/top.txt"] = &File{}

	var filled []string
	errCode := d.Readdir("/", fillCapture(&filled), 0, 0)

	require.Equal(t, 0, errCode, "Readdir should succeed")

	// Phantom basenames must NOT appear at root.
	assert.NotContains(t, filled, "git.adoc", "phantom: basename of nested remote file must not appear at root")
	assert.NotContains(t, filled, "README.md", "phantom: basename of nested remote file must not appear at root")
	assert.NotContains(t, filled, "beach.jpg", "phantom: basename of nested remote file must not appear at root")

	// Direct children SHOULD appear.
	assert.Contains(t, filled, "top.txt", "direct remote file should appear at root")
	assert.Contains(t, filled, "test_repo", "directory entry for nested remote path should appear at root")
	assert.Contains(t, filled, "photos", "directory entry for nested remote path should appear at root")
}

// TestReaddir_SubdirShowsOnlyOwnChildren verifies that listing a subdirectory
// only exposes that level's direct children, not deeper descendants.
func TestReaddir_SubdirShowsOnlyOwnChildren(t *testing.T) {
	localRoot := t.TempDir()

	// Create the local subdir so os.ReadDir succeeds.
	require.NoError(t, os.MkdirAll(localRoot+"/test_repo", 0o755))

	d := newTestDir(localRoot)
	d.RemoteFiles["/test_repo/Documentation/git.adoc"] = &File{}
	d.RemoteFiles["/test_repo/README.md"] = &File{}

	var filled []string
	errCode := d.Readdir("/test_repo", fillCapture(&filled), 0, 0)

	require.Equal(t, 0, errCode)

	// Deep descendant must NOT appear directly under /test_repo.
	assert.NotContains(t, filled, "git.adoc", "phantom: deep file must not appear at /test_repo level")

	// Direct children SHOULD appear.
	assert.Contains(t, filled, "README.md", "direct remote file under /test_repo should appear")
	assert.Contains(t, filled, "Documentation", "subdirectory entry should appear at /test_repo level")
}
