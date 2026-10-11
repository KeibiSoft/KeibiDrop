// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A friend's edit that keeps the byte length must reach a reader that already
// read the old bytes through its mount. Git does this on every commit: a branch
// ref is always 41 bytes. The tests below run on Linux, macOS and Windows, in
// the default mode (no live_collab), the mode git runs in.
// KEIBIDROP_LIVE_COLLAB=1 runs them in the live_collab mode. They are hermetic:
// no network, the repository comes from a local bare origin.

// gitVisibleWithin is how long a peer may lag the other side's git command. A
// notify lands in about a second. A stale view never lands at all.
const gitVisibleWithin = 20 * time.Second

// headCommit names the commit HEAD points at, read from the commit object, as
// git log does. rev-parse only resolves the ref: it answers before the object
// arrived, and the next git command then finds no commit ("a branch yet to be
// born", CI 2026-10-11).
const headCommit = "log -1 --format=%H"

// TestFUSEtoFUSE_SameSizeReplaceSeen replaces a file the reader holds with new
// bytes of the same length, in the two ways git and editors do it: a temp
// renamed over the file (git's lock file) and a rewrite in place. The reader
// read the old bytes, or wrote them itself (as when it made the branch). A
// plain folder and a .git folder each take their own page cache rule.
func TestFUSEtoFUSE_SameSizeReplaceSeen(t *testing.T) {
	p := connectFUSEPeers(t, os.Getenv("KEIBIDROP_LIVE_COLLAB") == "1")
	req := require.New(t)

	dirs := []string{"plain", "repo/.git/refs/heads"}
	for _, dir := range dirs {
		req.Equal("OK", p.alice.send(t, "mkdir_p "+dir, 10*time.Second))
		req.Equal("OK", p.bob.send(t, "wait_file "+dir+" 15", 20*time.Second))
	}

	// put writes content to file on a peer's mount, through a renamed temp or in place.
	put := func(t *testing.T, pr *testPeer, file, content string, viaRename bool) {
		t.Helper()
		if !viaRename {
			require.Equal(t, "OK", pr.send(t, "write_file "+file+" "+content, 10*time.Second))
			return
		}
		require.Equal(t, "OK", pr.send(t, "write_file "+file+".lock "+content, 10*time.Second))
		require.Equal(t, "OK", pr.send(t, "rename "+file+".lock "+file, 10*time.Second))
	}

	n := 0
	for _, dir := range dirs {
		for _, bobWrote := range []bool{false, true} {
			for _, viaRename := range []bool{true, false} {
				n++
				file := fmt.Sprintf("%s/ref%d", dir, n)
				name := fmt.Sprintf("%s/bobWrote=%v/rename=%v", dir, bobWrote, viaRename)
				t.Run(name, func(t *testing.T) {
					oldBytes := strings.Repeat("a", 40)
					newBytes := strings.Repeat("b", 40)

					if bobWrote {
						put(t, p.bob, file, oldBytes, viaRename)
						waitForContent(t, p.alice, file, oldBytes, gitVisibleWithin)
					} else {
						put(t, p.alice, file, oldBytes, viaRename)
					}
					// The reader reads the old bytes through its mount, so its
					// kernel holds them when the friend's version arrives.
					waitForContent(t, p.bob, file, oldBytes, gitVisibleWithin)

					put(t, p.alice, file, newBytes, viaRename)
					waitForContent(t, p.bob, file, newBytes, gitVisibleWithin)
				})
			}
		}
	}
}

// TestFUSEtoFUSE_GitPingPong takes turns with git on one repository through
// both mounts. Each side must see the other's commits, branches and staged
// files, and must never commit on a stale parent. The first step replays the
// 2026-10-11 run that found the gap: the reader made the branch, the friend
// committed on it, and the reader's git log kept showing the old commit.
func TestFUSEtoFUSE_GitPingPong(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	origin := seedGitOrigin(t)
	p := connectFUSEPeers(t, os.Getenv("KEIBIDROP_LIVE_COLLAB") == "1")

	t.Run("AliceClones", func(t *testing.T) {
		resp := p.alice.send(t, "exec . git "+gitFlags("alice")+" clone -q "+origin+" repo", 120*time.Second)
		require.Truef(t, strings.HasPrefix(resp, "EXEC:0:"), "clone: %s", resp)
		WaitForFileOnMount(t, filepath.Join(p.bobMount, "repo", ".git", "refs", "heads", "main"), 30*time.Second)
		waitGit(t, p.bob, headCommit, gitOn(t, p.alice, "rev-parse HEAD"))
		// The whole checkout arrived: the index, the tree and the files.
		waitGit(t, p.bob, "--no-optional-locks status --porcelain", "")
	})

	t.Run("BobBranchesAliceCommits", func(t *testing.T) {
		gitOn(t, p.bob, "checkout -q -b new_branch_new_test")
		waitGit(t, p.alice, "rev-parse --abbrev-ref HEAD", "new_branch_new_test")
		require.Equal(t, "OK", p.alice.send(t, "write_file repo/rap.txt bratan", 10*time.Second))
		require.Equal(t, "OK", p.bob.send(t, "wait_file repo/rap.txt 15", 20*time.Second))
		gitOn(t, p.bob, "add rap.txt")
		waitGit(t, p.alice, "--no-optional-locks status --porcelain", "A  rap.txt")
		gitOn(t, p.alice, "commit -q -m Bobr")
		waitGit(t, p.bob, headCommit, gitOn(t, p.alice, "rev-parse HEAD"))
		waitGit(t, p.bob, "--no-optional-locks status --porcelain", "")
	})

	t.Run("AliceCommitsBobSees", func(t *testing.T) {
		require.Equal(t, "OK", p.alice.send(t, "write_file repo/cahca.txt chechacha", 10*time.Second))
		gitOn(t, p.alice, "add cahca.txt")
		gitOn(t, p.alice, "commit -q -m alice-one")
		waitGit(t, p.bob, headCommit, gitOn(t, p.alice, "rev-parse HEAD"))
		waitGit(t, p.bob, "--no-optional-locks status --porcelain", "")
	})

	t.Run("BobCommitsOnAlicesCommit", func(t *testing.T) {
		parent := gitOn(t, p.alice, "rev-parse HEAD")
		waitGit(t, p.bob, headCommit, parent)
		require.Equal(t, "OK", p.bob.send(t, "write_file repo/bob.txt from-bob", 10*time.Second))
		gitOn(t, p.bob, "add bob.txt")
		gitOn(t, p.bob, "commit -q -m bob-one")
		require.Equal(t, parent, gitOn(t, p.bob, "rev-parse HEAD~1"), "Bob committed on a stale parent")
		waitGit(t, p.alice, headCommit, gitOn(t, p.bob, "rev-parse HEAD"))
		waitGit(t, p.alice, "--no-optional-locks status --porcelain", "")
	})

	// HEAD keeps its length between two branch names of the same length.
	t.Run("SameLengthBranchSwitch", func(t *testing.T) {
		gitOn(t, p.alice, "checkout -q -b feature-a")
		waitGit(t, p.bob, "rev-parse --abbrev-ref HEAD", "feature-a")
		gitOn(t, p.bob, "checkout -q -b feature-b")
		waitGit(t, p.alice, "rev-parse --abbrev-ref HEAD", "feature-b")
		gitOn(t, p.alice, "checkout -q feature-a")
		waitGit(t, p.bob, "rev-parse --abbrev-ref HEAD", "feature-a")
	})

	// A tracked file rewritten at its own length: the file and the index keep their sizes.
	t.Run("SameLengthEditCommitted", func(t *testing.T) {
		waitForContent(t, p.bob, "repo/seed.txt", "version-one", gitVisibleWithin)
		require.Equal(t, "OK", p.alice.send(t, "write_file repo/seed.txt version-two", 10*time.Second))
		gitOn(t, p.alice, "add seed.txt")
		gitOn(t, p.alice, "commit -q -m same-length")
		waitGit(t, p.bob, headCommit, gitOn(t, p.alice, "rev-parse HEAD"))
		waitForContent(t, p.bob, "repo/seed.txt", "version-two", gitVisibleWithin)
		waitGit(t, p.bob, "--no-optional-locks status --porcelain", "")
	})

	t.Run("BothFsckClean", func(t *testing.T) {
		for name, pr := range map[string]*testPeer{"alice": p.alice, "bob": p.bob} {
			resp := pr.send(t, "exec repo git --no-optional-locks fsck --no-progress --no-dangling", 120*time.Second)
			require.Truef(t, strings.HasPrefix(resp, "EXEC:0:"), "%s fsck: %s", name, resp)
		}
	})

	// The same run went on: Bob removed the repository, then Alice's rm -r
	// failed with "File exists". A sidecar the mount hides kept the folder.
	t.Run("BobRemovesAliceRemovesToo", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			// The delete of a fetched git object fails while this program still
			// holds it open without delete sharing ("being used by another
			// process", 2026-10-11). A Windows bug of its own, not this one.
			t.Skip("Windows: a fetched file still open refuses its delete (sharing violation)")
		}
		rmRepo := "exec . rm -rf repo"
		resp := p.bob.send(t, rmRepo, 60*time.Second)
		require.Truef(t, strings.HasPrefix(resp, "EXEC:0:"), "bob rm: %s", resp)
		WaitForCondition(t, gitVisibleWithin, 500*time.Millisecond, func() bool {
			_, err := os.Stat(filepath.Join(p.aliceMount, "repo", "seed.txt"))
			return os.IsNotExist(err)
		}, "Bob's delete reaches Alice")
		resp = p.alice.send(t, rmRepo, 60*time.Second)
		require.Truef(t, strings.HasPrefix(resp, "EXEC:0:"), "alice rm after bob's: %s", resp)
		_, err := os.Stat(filepath.Join(p.aliceMount, "repo"))
		require.True(t, os.IsNotExist(err), "repo still on Alice's mount")
	})
}

// gitFlags pins what the user's git config would change: the identity, no
// signing, and no line ending conversion (a converted file changes length).
func gitFlags(who string) string {
	return "-c user.name=" + who + " -c user.email=" + who + "@test -c commit.gpgsign=false -c core.autocrlf=false"
}

// gitOn runs git in the repository on a peer's mount and returns its output.
func gitOn(t *testing.T, pr *testPeer, args string) string {
	t.Helper()
	resp := pr.send(t, "exec repo git "+gitFlags("peer")+" "+args, 60*time.Second)
	require.Truef(t, strings.HasPrefix(resp, "EXEC:0:"), "git %s: %s", args, resp)
	return strings.TrimPrefix(resp, "EXEC:0:")
}

// waitGit polls git on a peer until its output equals want: the other side's
// change reached this mount. It fails with the last output it saw.
func waitGit(t *testing.T, pr *testPeer, args, want string) {
	t.Helper()
	deadline := time.Now().Add(gitVisibleWithin)
	var last string
	for time.Now().Before(deadline) {
		last = pr.send(t, "exec repo git "+gitFlags("peer")+" "+args, 30*time.Second)
		if last == "EXEC:0:"+want {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("git %s never became %q (last: %q)", args, want, last)
}

// seedGitOrigin makes a bare repository with one commit outside the mounts, so
// the test clones without the network. seed.txt has no newline: the test
// rewrites it at the same length through write_file.
func seedGitOrigin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	origin := filepath.Join(root, "origin.git")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append(strings.Fields(gitFlags("seed")), args...)...) // #nosec G204 -- test fixture
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
	}
	require.NoError(t, os.MkdirAll(work, 0o755))
	git(work, "init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte("version-one"), 0o644)) //nolint:gosec // G306: fixture
	git(work, "add", "seed.txt")
	git(work, "commit", "-q", "-m", "seed")
	git(root, "clone", "-q", "--bare", work, origin)
	return origin
}
