// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file tests that a real initdb run on a FUSE mount syncs every file it
// leaves on disk to the peer (no live file lost to a transient REMOVE).

package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// findInitdb locates an initdb binary (not on PATH on Debian/Ubuntu).
func findInitdb() string {
	if p, err := exec.LookPath("initdb"); err == nil {
		return p
	}
	for _, p := range []string{
		"/usr/lib/postgresql/16/bin/initdb",
		"/usr/lib/postgresql/15/bin/initdb",
		"/usr/local/bin/initdb",
	} {
		if exists(p) {
			return p
		}
	}
	return ""
}

// TestFUSEtoFUSE_InitdbPeerCompleteness runs a real initdb on Alice's FUSE mount
// and asserts the peer ends up with every file initdb left on disk. initdb does
// many create-delete-recreate bursts; the guard is that no transient REMOVE
// deletes a live file on the peer. Skips where initdb is absent (e.g. CI).
func TestFUSEtoFUSE_InitdbPeerCompleteness(t *testing.T) {
	skipIfNoFUSE(t)
	initdb := findInitdb()
	if initdb == "" {
		t.Skip("initdb not found")
	}
	// postgres' initdb refuses to run as root; the test box (and the test fleet) SSH as
	// root, so skip there rather than fail. Covered on any non-root host (e.g. the Mac).
	if os.Geteuid() == 0 {
		t.Skip("initdb refuses to run as root")
	}

	tp := SetupFUSEPeerPair(t, 180*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	pgdata := filepath.Join(tp.AliceMountDir, "pgdata")
	out, err := exec.Command(initdb, "-D", pgdata, "--no-locale", "-E", "UTF8").CombinedOutput()
	require.NoError(t, err, "initdb on the FUSE mount failed: %s", out)

	// The files initdb actually LEFT on disk (final state on Alice's mount).
	want := map[string]bool{}
	require.NoError(t, filepath.WalkDir(pgdata, func(p string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			rel, _ := filepath.Rel(tp.AliceMountDir, p)
			want["/"+filepath.ToSlash(rel)] = true
		}
		return nil
	}))
	t.Logf("initdb left %d files on Alice's mount", len(want))
	require.NotEmpty(t, want, "initdb produced no files; harness problem")

	// All of Bob's RemoteFiles come from this initdb (fresh pair), so the count is
	// the live-file-loss signal. Poll until it settles or reaches parity.
	bobCount := func() int {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		return len(tp.Bob.SyncTracker.RemoteFiles)
	}
	var last, stable int
	for i := 0; i < 45; i++ {
		time.Sleep(1 * time.Second)
		c := bobCount()
		if c >= len(want) {
			break
		}
		if c == last {
			if stable++; stable >= 6 {
				break // unchanged for 6s -> settled below parity
			}
		} else {
			stable = 0
		}
		last = c
	}

	got := bobCount()
	missing := []string{}
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	for f := range want {
		_, ok := tp.Bob.SyncTracker.RemoteFiles[f]
		if !ok {
			_, ok = tp.Bob.SyncTracker.RemoteFiles[f[1:]] // tolerate a missing leading slash
		}
		if !ok && len(missing) < 15 {
			missing = append(missing, f)
		}
	}
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()

	t.Logf("alice_files=%d peer_files=%d shortfall=%d", len(want), got, len(want)-got)
	if len(missing) > 0 {
		t.Logf("sample missing (on Alice's disk, absent on peer): %v", missing)
	}
	require.GreaterOrEqual(t, got, len(want),
		"peer is missing files that initdb left on disk: live-file loss")
}
