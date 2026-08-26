// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Directory moves across two FUSE peers: the sequential control case,
// ABOUTME: then the concurrent cross-move (mv A B/ vs mv B A/) cycle repro.

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSEtoFUSE_DirectoryMoves pins directory-rename behavior across peers.
// Directory renames travel as RENAME_FILE on the wire; both sides re-key the
// moved directory's children (or the old paths serve forever as ghosts and
// phantoms). The sequential subtest is the control: a one-sided move keeps
// children reachable and retires the old path. The concurrent subtest is the
// classic tree-invariant violation (VMCAI 2018: move is the one operation
// that unsynchronised breaks the tree): mv A B/ crossing mv B A/. The
// fingerprint rank picks ONE move on both machines: the loser undoes its own
// and applies the winner's, the winner skips the peer's, and both trees
// converge with no MkdirAll resurrection. Content loss is asserted
// unconditionally. Set KD_DM_LOGDIR to keep the peers' logs.
func TestFUSEtoFUSE_DirectoryMoves(t *testing.T) {
	skipIfNoFUSE(t)
	if runtime.GOOS == "windows" {
		t.Skip("unix shell forms; the Windows variant needs verb forms for mkdir/find")
	}
	if testing.Short() {
		t.Skip("two-FUSE spawned test skipped in short mode")
	}

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := newMountPoint(t)
	bobMount := newMountPoint(t)

	logDir := os.Getenv("KD_DM_LOGDIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	peerEnv := func(in, out int, mount string) map[string]string {
		return map[string]string{
			"RELAY_URL":     relay.URL(),
			"INBOUND_PORT":  fmt.Sprintf("%d", in),
			"OUTBOUND_PORT": fmt.Sprintf("%d", out),
			"MOUNT_DIR":     mount,
			"SAVE_DIR":      t.TempDir(),
			"USE_FUSE":      "1",
			"LOG_FILE":      filepath.Join(logDir, filepath.Base(filepath.Dir(mount))+".log"),
		}
	}
	alice := spawnPeer(t, binary, peerEnv(
		getFreePortInRange(t, 26700, 26749), getFreePortInRange(t, 26750, 26799), aliceMount))
	bob := spawnPeer(t, binary, peerEnv(
		getFreePortInRange(t, 26800, 26849), getFreePortInRange(t, 26850, 26899), bobMount))

	t.Cleanup(func() {
		for _, p := range []*testPeer{alice, bob} {
			fmt.Fprintln(p.stdin, "quit")
			done := make(chan error, 1)
			go func() { done <- p.cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = p.cmd.Process.Kill()
				<-done
			}
		}
		for _, dir := range []string{aliceMount, bobMount} {
			forceUnmount(dir)
			waitForUnmount(dir, 5*time.Second)
		}
	})

	require := require.New(t)

	aliceFP := strings.TrimPrefix(alice.send(t, "fingerprint", 5*time.Second), "FP:")
	bobFP := strings.TrimPrefix(bob.send(t, "fingerprint", 5*time.Second), "FP:")
	require.Equal("OK", alice.send(t, "register "+bobFP, 5*time.Second))
	require.Equal("OK", bob.send(t, "register "+aliceFP, 5*time.Second))

	aliceConn := alice.sendAsync(t, "create")
	WaitForCondition(t, 10*time.Second, 100*time.Millisecond, func() bool {
		return relay.EntryCount() > 0
	}, "waiting for relay registration")
	require.Equal("CONNECTED", bob.send(t, "join", 30*time.Second))
	require.Equal("CONNECTED", <-aliceConn)

	waitForFUSEMount(t, aliceMount, 15*time.Second)
	waitForFUSEMount(t, bobMount, 15*time.Second)

	ex := func(t *testing.T, p *testPeer, cmdline string, timeout time.Duration) string {
		t.Helper()
		resp := p.send(t, "exec . "+cmdline, timeout)
		if !strings.HasPrefix(resp, "EXEC:0:") {
			t.Fatalf("%q failed: %s", cmdline, resp)
		}
		return strings.TrimPrefix(resp, "EXEC:0:")
	}
	mustSend := func(t *testing.T, p *testPeer, verb string) {
		t.Helper()
		if resp := p.send(t, verb, 15*time.Second); resp != "OK" {
			t.Fatalf("%q failed: %s", verb, resp)
		}
	}

	// treeOf returns the sorted relative paths of every regular file under the
	// mount, read through the peer's own exec so the peer's FUSE view is what
	// is listed, not the host's cached view. A stale map entry can leave a
	// PHANTOM directory (readdir lists it, stat fails), which makes find exit
	// 1 while still printing everything it reached; that is itself part of
	// the demonstration, so any exit code is accepted and phantoms are kept
	// as entries.
	treeOf := func(t *testing.T, p *testPeer) []string {
		t.Helper()
		resp := p.send(t, "exec . find . -type f", 20*time.Second)
		if !strings.HasPrefix(resp, "EXEC:") {
			t.Fatalf("find failed to run: %s", resp)
		}
		body := strings.TrimPrefix(resp, "EXEC:")
		if i := strings.Index(body, ":"); i >= 0 {
			if code := body[:i]; code != "0" {
				t.Logf("find exit=%s (phantom entries present)", code)
			}
			body = body[i+1:]
		}
		var files []string
		for _, line := range strings.Split(body, "\\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "find:") {
				// "find: ./B: No such file or directory": keep the phantom
				// as a marker entry so tree comparison sees it.
				if j := strings.Index(line, ": "); j >= 0 {
					rest := line[j+2:]
					if k := strings.Index(rest, ":"); k > 0 {
						files = append(files, "PHANTOM:"+strings.TrimPrefix(rest[:k], "./"))
					}
				}
				continue
			}
			line = strings.TrimPrefix(line, "./")
			if line != "" && line != "." {
				files = append(files, line)
			}
		}
		sort.Strings(files)
		return files
	}

	readAt := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		return strings.TrimSpace(ex(t, p, "cat "+rel, 15*time.Second))
	}

	// The control: a one-sided directory rename must keep children reachable
	// on the peer. This is the baseline the concurrent case is judged against.
	t.Run("SequentialDirRenameKeepsChildren", func(t *testing.T) {
		ex(t, bob, "mkdir -p projA", 15*time.Second)
		mustSend(t, bob, "write_file projA/f1.txt hello-from-projA")
		WaitForFileOnMount(t, filepath.Join(aliceMount, "projA", "f1.txt"), 60*time.Second)
		if got := readAt(t, alice, "projA/f1.txt"); got != "hello-from-projA" {
			t.Fatalf("pre-move read through Alice's mount: got %q", got)
		}

		ex(t, bob, "mv projA projB", 15*time.Second)

		WaitForFileOnMount(t, filepath.Join(aliceMount, "projB", "f1.txt"), 60*time.Second)
		if got := readAt(t, alice, "projB/f1.txt"); got != "hello-from-projA" {
			t.Fatalf("child must stay readable through the peer's mount after a directory rename: got %q", got)
		}

		// Ghost check: the OLD child path must disappear on the peer. Before
		// the child re-keying it stayed visible forever (measured 2026-08-26).
		ghostGone := func() bool {
			_, err := os.Stat(filepath.Join(aliceMount, "projA", "f1.txt"))
			return os.IsNotExist(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for !ghostGone() && time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
		}
		// Cleanup for the next subtest before any verdict.
		ex(t, bob, "rm -rf projB", 15*time.Second)
		WaitForFileAbsent(t, filepath.Join(aliceMount, "projB", "f1.txt"), 30*time.Second)
		if !ghostGone() {
			t.Fatalf("old child path projA/f1.txt must disappear on the peer after the directory rename")
		}
	})

	// The cycle: mv A B/ on Alice crossing mv B A/ on Bob. A correct tree
	// keeps one arrangement on both peers; the shipped path re-creates the
	// departed parent via MkdirAll and the trees diverge.
	t.Run("ConcurrentCrossMoves", func(t *testing.T) {
		ex(t, bob, "mkdir -p A B", 15*time.Second)
		mustSend(t, bob, "write_file A/fa.txt content-of-fa")
		mustSend(t, bob, "write_file B/fb.txt content-of-fb")
		WaitForFileOnMount(t, filepath.Join(aliceMount, "A", "fa.txt"), 60*time.Second)
		WaitForFileOnMount(t, filepath.Join(aliceMount, "B", "fb.txt"), 60*time.Second)
		if got := readAt(t, alice, "A/fa.txt"); got != "content-of-fa" {
			t.Fatalf("seed read A/fa.txt through Alice's mount: got %q", got)
		}
		if got := readAt(t, alice, "B/fb.txt"); got != "content-of-fb" {
			t.Fatalf("seed read B/fb.txt through Alice's mount: got %q", got)
		}

		// The crossing moves, issued as close to simultaneously as the
		// stdin protocol allows.
		aliceMove := alice.sendAsync(t, "exec . mv A B")
		bobMove := bob.sendAsync(t, "exec . mv B A")
		aliceResp := <-aliceMove
		bobResp := <-bobMove
		t.Logf("alice mv A B -> %s", aliceResp)
		t.Logf("bob   mv B A -> %s", bobResp)

		// Let announces, renames and any repair settle.
		time.Sleep(15 * time.Second)

		aliceTree := treeOf(t, alice)
		bobTree := treeOf(t, bob)
		t.Logf("alice tree: %v", aliceTree)
		t.Logf("bob   tree: %v", bobTree)

		// Content preservation is unconditional: the bytes of both files
		// must exist somewhere on each peer, diverged tree or not.
		for _, want := range []struct{ name, content string }{
			{"fa.txt", "content-of-fa"},
			{"fb.txt", "content-of-fb"},
		} {
			for _, side := range []struct {
				peer *testPeer
				tree []string
				tag  string
			}{{alice, aliceTree, "alice"}, {bob, bobTree, "bob"}} {
				found := false
				for _, rel := range side.tree {
					if filepath.Base(rel) == want.name && readAt(t, side.peer, rel) == want.content {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("%s: %s with its bytes must exist somewhere after the crossing moves (tree: %v)", side.tag, want.name, side.tree)
				}
			}
		}

		converged := len(aliceTree) == len(bobTree)
		if converged {
			for i := range aliceTree {
				if aliceTree[i] != bobTree[i] {
					converged = false
					break
				}
			}
		}
		if !converged {
			t.Fatalf("both peers must converge to one tree after crossing directory moves: alice=%v bob=%v", aliceTree, bobTree)
		}
	})
}
