// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests isUnsafeRemotePath and the Notify guard that rejects a peer
// ABOUTME: path escaping the save root before it reaches the sync tracker.

package service

import (
	"context"
	"runtime"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsUnsafeRemotePath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		unsafe bool
	}{
		{"empty", "", true},
		{"parent-prefix", "../config/x", true},
		{"parent-nested", "a/../../x", true},
		{"absolute-unix", "/etc/passwd", false}, // absolute/leading-separator is kept under the save root by Join
		{"dot-dot-alone", "..", true},
		{"backslash-parent", "a\\..\\b", true},
		{"backslash-absolute", "\\config\\x", false}, // absolute/leading-separator is kept under the save root by Join
		// a/b/../c carries a literal ".." component; the helper rejects any
		// such component outright rather than resolving the net path, so
		// this is unsafe even though "b" cancels it out.
		{"forward-slash-internal-parent", "a/b/../c", true},
		{"plain-file", "doc.pdf", false},
		{"nested-safe", "sub/dir/file", false},
		// Starts with ".." but is not the exact segment "..": must not
		// false-positive on a Contains-style check.
		{"dotdot-like-prefix-not-exact", "..bashrc", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.unsafe, isUnsafeRemotePath(c.path), "path %q", c.path)
		})
	}
}

// newPathSafetyTestService builds a minimal KeibidropServiceImpl. Only
// Logger and SyncTracker are used, so this compiles and runs unchanged
// against either build variant's struct definition.
func newPathSafetyTestService() *KeibidropServiceImpl {
	return &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
}

func TestNotify_RefusesUnsafePath(t *testing.T) {
	svc := newPathSafetyTestService()

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE,
		Path: "../config/identity.enc",
		Name: "identity.enc",
		Attr: &bindings.Attr{Size: 10, ModificationTime: 1000},
	})
	require.ErrorIs(t, err, ErrGRPCUnsafePath)

	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok := svc.SyncTracker.RemoteFiles["../config/identity.enc"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()
	assert.False(t, ok, "unsafe path must not reach the tracker")
}

func TestNotify_RefusesUnsafeOldPathOnRename(t *testing.T) {
	svc := newPathSafetyTestService()

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: "../config/identity.enc",
		Path:    "renamed.txt",
	})
	require.ErrorIs(t, err, ErrGRPCUnsafePath)
}

func TestNotify_AcceptsSafePath(t *testing.T) {
	svc := newPathSafetyTestService()

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE,
		Path: "sub/dir/file.txt",
		Name: "file.txt",
		Attr: &bindings.Attr{Size: 10, ModificationTime: 1000},
	})
	require.NoError(t, err)

	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok := svc.SyncTracker.RemoteFiles["sub/dir/file.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()
	assert.True(t, ok, "safe path must be tracked normally")
}

// Windows strips trailing spaces and dots from each path component, so a
// segment like ".. " reaches the filesystem as "..". Reject anything made only
// of dots and spaces that carries at least two dots.
func TestIsUnsafeRemotePath_WindowsDotSpaceComponents(t *testing.T) {
	unsafe := []string{".. /evil", "a/.. /b", "...", "a/... /b", ". ./evil", ".. ", "..\\.. \\evil"}
	for _, p := range unsafe {
		assert.True(t, isUnsafeRemotePath(p), "must reject %q: normalizes to .. on Windows", p)
	}
	safe := []string{"..bashrc", "a..b", "file. txt", "a. b/c", "...leading", "normal.txt", "/a/b.txt"}
	for _, p := range safe {
		assert.False(t, isUnsafeRemotePath(p), "must accept legit name %q", p)
	}
}

// A peer path that resolves to the save root itself is never a legitimate target:
// the rename branch would drive os.Remove or RenameShared on the root directory.
func TestIsUnsafeRemotePath_RejectsRootResolvingPaths(t *testing.T) {
	for _, p := range []string{"/", ".", "./", "/.", "././", "\\", ".\\"} {
		assert.True(t, isUnsafeRemotePath(p), "must reject %q: resolves to the save root", p)
	}
	for _, p := range []string{"./a", "a", "/a/b.txt", ".hidden"} {
		assert.False(t, isUnsafeRemotePath(p), "must accept %q", p)
	}
}

// OldPath is mandatory for a rename. An empty one joins to the save root, so the
// general "empty OldPath is fine" exemption must not apply to rename types.
func TestIsUnsafeNotify_RenameRequiresSafeOldPath(t *testing.T) {
	for _, old := range []string{"", "/", ".", "../escape"} {
		assert.True(t, isUnsafeNotify(bindings.NotifyType_RENAME_FILE, "x", old),
			"rename with OldPath %q must be refused", old)
		assert.True(t, isUnsafeNotify(bindings.NotifyType_RENAME_DIR, "x", old),
			"rename dir with OldPath %q must be refused", old)
	}
	assert.False(t, isUnsafeNotify(bindings.NotifyType_RENAME_FILE, "new.txt", "old.txt"))
	// A non-rename legitimately carries no OldPath.
	assert.False(t, isUnsafeNotify(bindings.NotifyType_ADD_FILE, "a.txt", ""))
	assert.True(t, isUnsafeNotify(bindings.NotifyType_ADD_FILE, "a.txt", "../x"))
}

// Windows also strips a component made ONLY of dots and spaces to nothing, so
// "/. " or " " resolve to the parent, i.e. the save root. Reject those forms on
// every build. Names that merely SHRINK ("foo ") alias a sibling only on a
// Windows disk, so they are rejected there and stay legal on POSIX.
func TestIsUnsafeRemotePath_RejectsStripToNothingComponents(t *testing.T) {
	for _, p := range []string{" ", ". ", " . ", "/. ", "/ ", ".  ", "a/ /b", "a\\ \\b", "a/. "} {
		assert.True(t, isUnsafeRemotePath(p), "must reject %q: a component strips to nothing on Windows", p)
	}
	for _, p := range []string{"foo ", "foo.", "a/b. ", "dir /f"} {
		want := runtime.GOOS == "windows"
		assert.Equal(t, want, isUnsafeRemotePath(p), "aliasing name %q is Windows-only unsafe", p)
	}
}
