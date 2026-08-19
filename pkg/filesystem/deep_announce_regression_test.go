// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Regression test for the silent under-count of a cold recursive walk.
// ABOUTME: An announce for a deep file that arrives before its parent must stay reachable.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// announceTime is the fixed mtime every announcement in this file carries.
var announceTime = time.Unix(1755620294, 0)

// walkRemote lists path and every directory under it, the way a recursive
// walker does: read the level, then stat each name to find the directories.
func walkRemote(t *testing.T, d *Dir, path string) []string {
	t.Helper()
	var found []string
	var names []string
	require.Equal(t, 0, d.Readdir(path, fillCapture(&names), 0, 0), "readdir %s", path)
	for _, name := range names {
		if name == "." || name == ".." {
			continue
		}
		child := path + name
		if path != "/" {
			child = path + "/" + name
		}
		var st winfuse.Stat_t
		if d.Getattr(child, &st, ^uint64(0)) != 0 {
			continue // Not resolvable: a walker drops it and never descends.
		}
		if st.Mode&winfuse.S_IFMT == winfuse.S_IFDIR {
			found = append(found, walkRemote(t, d, child)...)
			continue
		}
		found = append(found, child)
	}
	return found
}

// TestDeepAnnounceBeforeParent_StaysReachable reproduces the 2026-08-19 DFIR
// pretest: the analyst's first cold recursive walk returned 23 of 24 files.
// The announce for the only depth-2 file arrived 0.82s before the announce
// that made its parent, single-level mkdir failed with ENOENT, and Getattr
// then refused the directory that Readdir still advertised.
func TestDeepAnnounceBeforeParent_StaysReachable(t *testing.T) {
	localRoot := t.TempDir()
	d := newTestDir(localRoot)

	// Announcement order taken from the pretest daemon log.
	require.NoError(t, d.AddRemoteFile(nopLogger(), "/nested/deeper/buried.txt", "buried.txt", remoteStat(17, announceTime)))
	require.NoError(t, d.AddRemoteFile(nopLogger(), "/nested/ioc-beta.log", "ioc-beta.log", remoteStat(46, announceTime)))
	require.NoError(t, d.AddRemoteFile(nopLogger(), "/note-1.txt", "note-1.txt", remoteStat(35, announceTime)))

	// Readdir advertises the deep directory, so Getattr must resolve it.
	var names []string
	require.Equal(t, 0, d.Readdir("/nested", fillCapture(&names), 0, 0))
	assert.Contains(t, names, "deeper", "readdir must list the deep directory")

	var st winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/nested/deeper", &st, ^uint64(0)),
		"getattr must resolve a directory that readdir advertises")
	assert.Equal(t, uint32(winfuse.S_IFDIR), st.Mode&winfuse.S_IFMT, "must resolve as a directory")

	// The walk a triage tool runs must find every announced file.
	assert.ElementsMatch(t,
		[]string{"/note-1.txt", "/nested/ioc-beta.log", "/nested/deeper/buried.txt"},
		walkRemote(t, d, "/"),
		"a cold recursive walk must not drop a deep file")
}

// TestAnnounceParentFirst_StillWorks pins the ordinary order, so the fix for
// the out-of-order case cannot change the common path.
func TestAnnounceParentFirst_StillWorks(t *testing.T) {
	localRoot := t.TempDir()
	d := newTestDir(localRoot)

	require.NoError(t, d.AddRemoteFile(nopLogger(), "/nested/ioc-beta.log", "ioc-beta.log", remoteStat(46, announceTime)))
	require.NoError(t, d.AddRemoteFile(nopLogger(), "/nested/deeper/buried.txt", "buried.txt", remoteStat(17, announceTime)))

	assert.ElementsMatch(t,
		[]string{"/nested/ioc-beta.log", "/nested/deeper/buried.txt"},
		walkRemote(t, d, "/"))
}

// TestMkdirInternal_RealErrorStillFails: the parent-chain retry must not turn
// a genuine mkdir failure into a success.
func TestMkdirInternal_RealErrorStillFails(t *testing.T) {
	localRoot := t.TempDir()
	d := newTestDir(localRoot)

	// A path whose parent is a regular file cannot become a directory.
	require.NoError(t, d.AddRemoteFile(nopLogger(), "/blocker", "blocker", remoteStat(3, announceTime)))
	var st winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/blocker", &st, ^uint64(0)))

	require.NoError(t, os.WriteFile(filepath.Join(localRoot, "blocker"), []byte("no"), 0o644))
	assert.NotEqual(t, 0, d.MkdirFromPeer("/blocker/child", 0755), "mkdir under a file must still fail")
}

// TestMkdirFromPeer_NoChainOutsideRoot: a peer path that climbs out of
// save_path must never get a directory chain made for it.
func TestMkdirFromPeer_NoChainOutsideRoot(t *testing.T) {
	parent := t.TempDir()
	localRoot := filepath.Join(parent, "save")
	require.NoError(t, os.MkdirAll(localRoot, 0o755))
	d := newTestDir(localRoot)

	assert.NotEqual(t, 0, d.MkdirFromPeer("../escape/a/b", 0755), "escaping mkdir must fail")

	_, err := os.Stat(filepath.Join(parent, "escape"))
	assert.True(t, os.IsNotExist(err), "no directory may be made outside save_path")
}
