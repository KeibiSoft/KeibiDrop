// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package tests

import (
	"crypto/md5" //#nosec G501 -- integrity compare, not security
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSEtoFUSE_GitClone_OnDemandIntegrity reproduces the reported corruption:
// peer A (alice) clones a real repo into her mount; the clone FINISHES; then
// (after a delay, so every debounced/batched notification has flushed) peer B
// (bob) reads the repo on-demand and runs the exact command that failed in the
// field — `git status` — which returned "fatal: Bad object HEAD".
//
// The existing TestFUSEtoFUSE_GitClone only runs git log/status/cat-HEAD on a
// handful of objects; this one verifies EVERY object (git fsck --full) and then
// byte-compares every file A has against what B serves on-demand, to pinpoint
// which file (pack/idx/ref/object) came across wrong.
func TestFUSEtoFUSE_GitClone_OnDemandIntegrity(t *testing.T) {
	skipIfNoFUSE(t)
	p := connectFUSEPeers(t, false) // on-demand (prefetch off = current default)
	req := require.New(t)

	const repoURL = "https://github.com/KeibiSoft/go-fp.git"

	// A clones into her mount: a burst of create/write/rename/remove (pack +
	// idx temp→rename, refs, HEAD, working tree).
	resp := p.alice.send(t, "exec . git clone --quiet "+repoURL+" repo", 180*time.Second)
	req.Truef(strings.HasPrefix(resp, "EXEC:0:"), "alice clone failed: %s", resp)
	t.Log("alice: clone finished")

	// Wait for the tree to propagate to B, then a fixed delay so the clone is
	// long done and all notifications have flushed before B reads anything.
	WaitForFileOnMount(t, filepath.Join(p.bobMount, "repo", ".git", "HEAD"), 30*time.Second)
	WaitForFileOnMount(t, filepath.Join(p.bobMount, "repo", "go.mod"), 30*time.Second)
	time.Sleep(8 * time.Second)

	// Pinpoint FIRST, before B runs any git command (git status rewrites
	// .git/index, which would otherwise show as a false mismatch). This is the
	// pure synced/on-demand state: every file A has vs what B serves on-demand.
	mismatches := diffTrees(t, filepath.Join(p.aliceMount, "repo"), filepath.Join(p.bobMount, "repo"))
	for _, m := range mismatches {
		t.Logf("MISMATCH: %s", m)
	}

	// His exact failing command.
	status := p.bob.send(t, "exec repo git status", 60*time.Second)
	t.Logf("bob `git status`: %s", status)

	// Full object verification (reads + checks every object on-demand).
	fsck := p.bob.send(t, "exec repo git fsck --full --strict", 120*time.Second)
	t.Logf("bob `git fsck`: %s", fsck)

	revparse := p.bob.send(t, "exec repo git rev-parse HEAD", 30*time.Second)
	t.Logf("bob `git rev-parse HEAD`: %s", revparse)

	// Assertions — any of these failing IS the bug, reproduced.
	req.Falsef(strings.Contains(status, "Bad object") || strings.Contains(status, "fatal"),
		"git status reports corruption: %s", status)
	req.Truef(strings.HasPrefix(status, "EXEC:0:"), "git status nonzero exit: %s", status)
	req.Truef(strings.HasPrefix(fsck, "EXEC:0:"), "git fsck reports corruption: %s", fsck)
	req.Emptyf(mismatches, "%d file(s) diverged between A (source) and B (on-demand)", len(mismatches))
}

// diffTrees walks src, and for every regular file compares its md5 to the same
// file under dst (read on-demand through B's FUSE mount). Returns a list of
// "relpath: srcSize vs dstSize (md5 a/b)" for files that differ or are missing.
func diffTrees(t *testing.T, src, dst string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(src, path)
		a, aerr := os.ReadFile(path) //#nosec G304 -- test paths
		if aerr != nil {
			return nil
		}
		b, berr := os.ReadFile(filepath.Join(dst, rel)) //#nosec G304 -- test paths
		if berr != nil {
			out = append(out, fmt.Sprintf("%s: MISSING on B (%v)", rel, berr))
			return nil
		}
		if md5.Sum(a) != md5.Sum(b) { //#nosec G401
			out = append(out, fmt.Sprintf("%s: A=%dB md5=%x  B=%dB md5=%x",
				rel, len(a), md5.Sum(a), len(b), md5.Sum(b)))
		}
		return nil
	})
	return out
}
