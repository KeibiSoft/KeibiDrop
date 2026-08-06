// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import "testing"

// newScopeFS builds a minimal FS whose Root holds one cached remote file, so a test
// can assert clear-vs-preserve behavior without a real mount or peer.
func newScopeFS() *FS {
	fs := NewFS(nopLogger())
	root := &Dir{
		RelativePath:     "/",
		AllFileMap:       map[string]*File{"/x": {}},
		AllDirMap:        map[string]*Dir{},
		RemoteFiles:      map[string]*File{"/x": {}},
		OpenFileHandlers: map[uint64]*HandleEntry{},
	}
	root.Root = root
	root.logger = nopLogger()
	root.SetCtx(fs.ctx)
	fs.SetRoot(root)
	return fs
}

func remoteCount(fs *FS) int {
	fs.Root().RemoteFilesLock.RLock()
	defer fs.Root().RemoteFilesLock.RUnlock()
	return len(fs.Root().RemoteFiles)
}

// The first peer adopts the cache without clearing; the SAME peer keeps it across
// repeat calls (a reconnect or a resumed session); a DIFFERENT peer drops it so one
// peer's artifacts never leak to the next. An empty fingerprint never wipes.
func TestEnsurePeerScope(t *testing.T) {
	fs := newScopeFS()
	if remoteCount(fs) != 1 {
		t.Fatalf("setup: want 1 cached remote file, got %d", remoteCount(fs))
	}

	fs.EnsurePeerScope("peerA") // first peer: adopt, do NOT clear
	if got := remoteCount(fs); got != 1 {
		t.Fatalf("first peer must not clear: got %d remote files, want 1", got)
	}

	fs.EnsurePeerScope("peerA") // same peer (reconnect/resume): keep
	if got := remoteCount(fs); got != 1 {
		t.Fatalf("same peer must keep the cache: got %d, want 1", got)
	}

	fs.EnsurePeerScope("") // unknown identity: never a spurious wipe
	if got := remoteCount(fs); got != 1 {
		t.Fatalf("empty fingerprint must not clear: got %d, want 1", got)
	}

	fs.EnsurePeerScope("peerB") // different peer: drop the prior peer's view
	if got := remoteCount(fs); got != 0 {
		t.Fatalf("different peer must clear the cache (no cross-peer leak): got %d, want 0", got)
	}

	// And the new owner is now adopted — re-announcing peerB keeps the (empty) cache.
	fs.EnsurePeerScope("peerB")
	if got := remoteCount(fs); got != 0 {
		t.Fatalf("re-adopted owner must not re-clear: got %d, want 0", got)
	}
}

// CancelInFlight (explicit disconnect) PRESERVES the cached view so a same-peer
// reconnect resumes with no re-fetch, and installs a fresh live context. ClearFiles
// (the different-peer / unmount path) wipes it. This is the whole point of #2: an
// explicit disconnect must not behave like a peer switch.
func TestCancelInFlightPreservesCache(t *testing.T) {
	fs := newScopeFS()

	oldCtx := fs.ctx
	fs.CancelInFlight()
	if got := remoteCount(fs); got != 1 {
		t.Fatalf("CancelInFlight must preserve the cached view: got %d, want 1", got)
	}
	if oldCtx.Err() == nil {
		t.Fatal("CancelInFlight must cancel the in-flight (old) context")
	}
	if fs.ctx == oldCtx || fs.ctx.Err() != nil {
		t.Fatal("CancelInFlight must install a fresh, live context for the next session")
	}

	fs.ClearFiles()
	if got := remoteCount(fs); got != 0 {
		t.Fatalf("ClearFiles must wipe the cached view: got %d, want 0", got)
	}
}
