// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file guards the postgres/git handoff pattern over FUSE sync: a file
// rewritten via delete+recreate, and a lockfile deletion, must not take live
// files down on the peer (the PR #177 REMOVE-debounce concern).

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSEtoFUSE_CreateDeleteRecreateKeepsFile rewrites files via the
// create->delete->recreate burst that initdb and git do, and asserts the peer
// ends with every recreated file present. The receiver buffers a REMOVE for ~1s;
// the recreate's ADD must win that race so the file is not left deleted.
func TestFUSEtoFUSE_CreateDeleteRecreateKeepsFile(t *testing.T) {
	skipIfNoFUSE(t)
	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	dir := filepath.Join(tp.AliceMountDir, "db")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	const n = 30
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f_%d", i))
		require.NoError(t, os.WriteFile(p, []byte("v1"), 0o600))
		require.NoError(t, os.Remove(p))
		require.NoError(t, os.WriteFile(p, []byte("final"), 0o600))
	}

	// Settle past the sender debounce (200ms) + receiver REMOVE buffer (~1s).
	time.Sleep(4 * time.Second)

	missing := []string{}
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("/db/f_%d", i)
		if _, ok := tp.Bob.SyncTracker.RemoteFiles[key]; !ok {
			missing = append(missing, key)
		}
	}
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	require.Empty(t, missing, "create-delete-recreate left files deleted on the peer: %v", missing)
}

// TestFUSEtoFUSE_RemoveLockfileKeepsNeighbors mirrors the handoff's lockfile step:
// with a set of live files plus a postmaster.pid synced to the peer, deleting only
// the lockfile must remove just the lockfile and leave every live file intact.
func TestFUSEtoFUSE_RemoveLockfileKeepsNeighbors(t *testing.T) {
	skipIfNoFUSE(t)
	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	dir := filepath.Join(tp.AliceMountDir, "pgdata")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	const n = 40
	for i := 0; i < n; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(dir, fmt.Sprintf("rel_%d", i)),
			[]byte(fmt.Sprintf("data-%d", i)), 0o600))
	}
	lock := filepath.Join(dir, "postmaster.pid")
	require.NoError(t, os.WriteFile(lock, []byte("4242\n/pgdata\n"), 0o600))

	// Wait until the lockfile and the last live file have surfaced on the peer.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "/pgdata/postmaster.pid", 30*time.Second)
	WaitForRemoteFile(t, tp.Bob.SyncTracker, fmt.Sprintf("/pgdata/rel_%d", n-1), 30*time.Second)

	// The handoff step: delete ONLY the lockfile.
	require.NoError(t, os.Remove(lock))

	// The lockfile must disappear on the peer.
	WaitForCondition(t, 10*time.Second, 100*time.Millisecond, func() bool {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		_, ok := tp.Bob.SyncTracker.RemoteFiles["/pgdata/postmaster.pid"]
		return !ok
	}, "lockfile removed on peer")

	// Every live file must remain. Give the REMOVE buffer time to wrongly fire.
	time.Sleep(2 * time.Second)
	missing := []string{}
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("/pgdata/rel_%d", i)
		if _, ok := tp.Bob.SyncTracker.RemoteFiles[key]; !ok {
			missing = append(missing, key)
		}
	}
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	require.Empty(t, missing, "deleting the lockfile took down live neighbors on the peer: %v", missing)
}
