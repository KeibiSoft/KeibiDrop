// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Bidirectional-edit conformance (docs/REAL_APP_FUSE_TEST_PLAN.md §2): the owner edits a
// file through its own mount with a given write pattern; the reader must converge to the
// same bytes. Atomic-save-via-rename is the pattern regular apps use: copy into a working
// file, then swap it over the target. Each subtest logs its convergence time; the matrix
// in research_artefacts/ is built from these numbers.
func TestFUSEtoFUSE_BidirectionalEditPatterns(t *testing.T) {
	skipIfNoFUSE(t)

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := newMountPoint(t)
	bobMount := newMountPoint(t)

	logDir := os.Getenv("KD_BIDIR_LOGDIR")
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
			// The collab profile is the one under test: in-place edits need auto_cache on macOS.
			"KEIBIDROP_LIVE_COLLAB": "1",
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

	ex := func(t *testing.T, p *testPeer, dir, cmdline string, timeout time.Duration) string {
		t.Helper()
		resp := p.send(t, "exec "+dir+" "+cmdline, timeout)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "%q failed: %s", cmdline, resp)
		return strings.TrimPrefix(resp, "EXEC:0:")
	}

	// Windows has no dd/mv/cp/ls/cat/md5sum: verb forms drive the same FUSE
	// surface through the peer's own mount. Unix keeps the shell forms — the
	// tools ARE the measured pattern.
	useVerbs := runtime.GOOS == "windows"
	writeRand := func(t *testing.T, p *testPeer, rel string, n int) {
		t.Helper()
		if useVerbs {
			require.Equal("OK", p.send(t, fmt.Sprintf("write_rand %s %d", rel, n), 60*time.Second))
			return
		}
		ex(t, p, ".", fmt.Sprintf("dd if=/dev/urandom of=%s bs=%d count=1", rel, n), 60*time.Second)
	}
	// off must be a multiple of n in the dd form (block-addressed seek).
	writeRandAt := func(t *testing.T, p *testPeer, rel string, off int64, n int) {
		t.Helper()
		if useVerbs {
			require.Equal("OK", p.send(t, fmt.Sprintf("write_rand_at %s %d %d", rel, off, n), 60*time.Second))
			return
		}
		ex(t, p, ".", fmt.Sprintf("dd if=/dev/urandom of=%s bs=%d count=1 seek=%d conv=notrunc", rel, n, off/int64(n)), 60*time.Second)
	}
	renameOver := func(t *testing.T, p *testPeer, oldRel, newRel string) {
		t.Helper()
		if useVerbs {
			require.Equal("OK", p.send(t, "rename "+oldRel+" "+newRel, 15*time.Second))
			return
		}
		ex(t, p, ".", "mv -f "+oldRel+" "+newRel, 15*time.Second)
	}
	copyFile := func(t *testing.T, p *testPeer, src, dst string) {
		t.Helper()
		if useVerbs {
			require.Equal("OK", p.send(t, "copy "+src+" "+dst, 60*time.Second))
			return
		}
		ex(t, p, ".", "cp "+src+" "+dst, 60*time.Second)
	}
	listNames := func(t *testing.T, p *testPeer) []string {
		t.Helper()
		if useVerbs {
			lines := p.sendMultiLine(t, "list_dir .", "END", 15*time.Second)
			var out []string
			for _, l := range lines {
				if strings.HasPrefix(l, "ENTRY:") {
					out = append(out, strings.TrimPrefix(l, "ENTRY:"))
				}
			}
			return out
		}
		return strings.Split(ex(t, p, ".", "ls", 15*time.Second), "\\n")
	}
	readFile := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		if useVerbs {
			resp := p.send(t, "read_file "+rel, 15*time.Second)
			require.True(strings.HasPrefix(resp, "DATA:"), "read_file failed: %s", resp)
			parts := strings.SplitN(resp, ":", 3)
			require.Len(parts, 3)
			return strings.TrimSpace(parts[2])
		}
		return strings.TrimSpace(ex(t, p, ".", "cat "+rel, 15*time.Second))
	}
	md5cmd := "md5 -q"
	if runtime.GOOS == "linux" {
		md5cmd = "md5sum"
	}
	// A missing or unreadable file is NOT fatal here: rename/announce
	// re-keying can leave a sub-second visibility gap, and convergence is an
	// eventually property — waitConverged keeps polling. The sentinel embeds
	// the peer, so two absent reads never compare equal.
	fileMd5 := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		if useVerbs {
			resp := p.send(t, "md5 "+rel, 60*time.Second)
			if !strings.HasPrefix(resp, "MD5:") {
				return fmt.Sprintf("absent:%p", p)
			}
			return strings.TrimPrefix(resp, "MD5:")
		}
		resp := p.send(t, "exec . "+md5cmd+" "+rel, 30*time.Second)
		fields := strings.Fields(strings.TrimPrefix(resp, "EXEC:0:"))
		if !strings.HasPrefix(resp, "EXEC:0:") || len(fields) == 0 {
			return fmt.Sprintf("absent:%p", p)
		}
		return fields[0]
	}

	// waitConverged polls BOTH mounts until the reader's md5 equals the owner's.
	// The owner's sum is live, not frozen at call time: the owner's own view can
	// settle after a save (stat refresh, debounce), and convergence is a property
	// of two live systems. The empty-file sum never counts as convergence — no
	// flow here produces an empty file, so it marks a torn read.
	waitConverged := func(t *testing.T, reader, owner *testPeer, rel string, timeout time.Duration) time.Duration {
		t.Helper()
		const emptyMd5 = "d41d8cd98f00b204e9800998ecf8427e"
		start := time.Now()
		deadline := start.Add(timeout)
		var got, want string
		for time.Now().Before(deadline) {
			want = fileMd5(t, owner, rel)
			got = fileMd5(t, reader, rel)
			if got == want && want != emptyMd5 {
				elapsed := time.Since(start)
				t.Logf("converged in %v", elapsed.Round(10*time.Millisecond))
				return elapsed
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("no convergence after %v: owner=%s reader=%s", timeout, want, got)
		return 0
	}

	const f = "doc.bin"

	t.Run("Seed", func(t *testing.T) {
		writeRand(t, bob, f, 512*1024)
		WaitForFileOnMount(t, filepath.Join(aliceMount, f), 60*time.Second)
		waitConverged(t, alice, bob, f, 60*time.Second)
	})

	// The app-save pattern: write a working copy, swap it over the target.
	t.Run("AtomicRenameOverwrite", func(t *testing.T) {
		writeRand(t, bob, f+".tmp", 512*1024)
		renameOver(t, bob, f+".tmp", f)
		waitConverged(t, alice, bob, f, 60*time.Second)
	})

	// Same byte length, content changed: the in-place profile (databases, mmap writers).
	t.Run("InPlaceSameSize", func(t *testing.T) {
		writeRandAt(t, bob, f, 0, 4096)
		waitConverged(t, alice, bob, f, 60*time.Second)
	})

	t.Run("TruncateRewrite", func(t *testing.T) {
		writeRand(t, bob, f, 192*1024)
		waitConverged(t, alice, bob, f, 60*time.Second)
	})

	// The reverse direction: the reader edits the owner's file through its mount.
	t.Run("ReverseInPlace", func(t *testing.T) {
		writeRandAt(t, alice, f, 0, 4096)
		waitConverged(t, bob, alice, f, 60*time.Second)
	})

	// Rapid save cycles: three rename-overwrites back to back (editors autosaving).
	t.Run("RenameCycleBurst", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			writeRand(t, bob, f+".tmp", 512*1024)
			renameOver(t, bob, f+".tmp", f)
		}
		waitConverged(t, alice, bob, f, 90*time.Second)
	})

	// The app-save swap in REVERSE: the reader writes a working copy and swaps
	// it over the owner's file through its own mount.
	t.Run("ReverseRenameOverwrite", func(t *testing.T) {
		writeRand(t, alice, f+".new", 512*1024)
		renameOver(t, alice, f+".new", f)
		waitConverged(t, bob, alice, f, 60*time.Second)
	})

	// A plain reader-side rename of the owner's file: no rewrite follows, so
	// nothing can heal a swallowed announcement. The owner must see the move.
	t.Run("ReversePlainRename", func(t *testing.T) {
		renameOver(t, alice, f, f+".moved")
		WaitForFileOnMount(t, filepath.Join(bobMount, f+".moved"), 60*time.Second)
		waitConverged(t, bob, alice, f+".moved", 60*time.Second)
		// The old name must be gone on both sides.
		WaitForFileAbsent(t, filepath.Join(bobMount, f), 30*time.Second)
		renameOver(t, alice, f+".moved", f) // restore for later subtests
		WaitForFileOnMount(t, filepath.Join(bobMount, f), 60*time.Second)
	})

	// The owner renames its own file AFTER a reverse edit adopted it into the
	// remote map. The rename must still be announced (adoption must not mute
	// the owner).
	t.Run("OwnerRenameAfterAdoption", func(t *testing.T) {
		writeRandAt(t, alice, f, 0, 4096)
		waitConverged(t, bob, alice, f, 60*time.Second)
		renameOver(t, bob, f, f+".owner")
		WaitForFileOnMount(t, filepath.Join(aliceMount, f+".owner"), 60*time.Second)
		waitConverged(t, alice, bob, f+".owner", 60*time.Second)
	})

	// Both peers save divergent content inside one debounce window, so neither
	// edit saw the other (announced base < holder's
	// version). LWW converges the canonical name; the loser must survive as a
	// .conflict- sibling on BOTH peers. Turn-taking flows above prove the
	// negative: none of them may produce a conflict file.
	t.Run("ConcurrentEditConflictCopy", func(t *testing.T) {
		listConflicts := func(p *testPeer) []string {
			var out []string
			for _, name := range listNames(t, p) {
				if strings.Contains(name, "clash.conflict-") {
					out = append(out, strings.TrimSpace(name))
				}
			}
			return out
		}
		require.Equal("OK", bob.send(t, "write_file clash.txt seed-version", 10*time.Second))
		WaitForFileOnMount(t, filepath.Join(aliceMount, "clash.txt"), 60*time.Second)
		waitConverged(t, alice, bob, "clash.txt", 60*time.Second)

		// Divergent writes within the 200 ms debounce: provably concurrent.
		require.Equal("OK", bob.send(t, "write_file clash.txt bob-concurrent-version", 10*time.Second))
		require.Equal("OK", alice.send(t, "write_file clash.txt alice-concurrent-version", 10*time.Second))

		waitConverged(t, alice, bob, "clash.txt", 60*time.Second)
		WaitForCondition(t, 60*time.Second, 500*time.Millisecond, func() bool {
			return len(listConflicts(alice)) >= 1 && len(listConflicts(bob)) >= 1
		}, "waiting for the conflict copy on both peers")

		// The two versions both survive: canonical + conflict = both writes.
		canonical := readFile(t, bob, "clash.txt")
		conflicts := listConflicts(bob)
		require.Len(conflicts, 1, "exactly one conflict copy expected")
		preserved := readFile(t, bob, conflicts[0])
		got := []string{canonical, preserved}
		require.ElementsMatch([]string{"bob-concurrent-version", "alice-concurrent-version"}, got,
			"both concurrent versions must survive (canonical + conflict)")
		waitConverged(t, alice, bob, conflicts[0], 60*time.Second)
	})

	// The swap case: the reader swap-saves a WORKING COPY taken from version
	// v1 while the owner concurrently edits to v2. The rename announce
	// declares base=v1; the owner holds v2 with authority, so its bytes are
	// preserved as a conflict sibling instead of silently clobbered.
	t.Run("SwapSaveStaleBase", func(t *testing.T) {
		listConflicts := func(p *testPeer) []string {
			var out []string
			for _, name := range listNames(t, p) {
				if strings.Contains(name, "swapc.conflict-") {
					out = append(out, strings.TrimSpace(name))
				}
			}
			return out
		}
		require.Equal("OK", bob.send(t, "write_file swapc.txt owner-v1", 10*time.Second))
		WaitForFileOnMount(t, filepath.Join(aliceMount, "swapc.txt"), 60*time.Second)
		waitConverged(t, alice, bob, "swapc.txt", 60*time.Second)

		// Alice takes her working copy of v1.
		copyFile(t, alice, "swapc.txt", "swapc.txt.work")
		require.Equal("OK", alice.send(t, "write_file swapc.txt.work reader-swap-version", 10*time.Second))

		// Concurrent finish: owner's in-place edit debounces 200 ms; the
		// reader's swap RENAME announces immediately and crosses it.
		require.Equal("OK", bob.send(t, "write_file swapc.txt owner-v2-concurrent", 10*time.Second))
		renameOver(t, alice, "swapc.txt.work", "swapc.txt")

		waitConverged(t, alice, bob, "swapc.txt", 60*time.Second)
		WaitForCondition(t, 60*time.Second, 500*time.Millisecond, func() bool {
			return len(listConflicts(alice)) >= 1 && len(listConflicts(bob)) >= 1
		}, "waiting for the swap conflict copy on both peers")

		canonical := readFile(t, bob, "swapc.txt")
		conflicts := listConflicts(bob)
		require.Len(conflicts, 1, "exactly one conflict copy expected")
		preserved := readFile(t, bob, conflicts[0])
		require.ElementsMatch([]string{"owner-v2-concurrent", "reader-swap-version"},
			[]string{canonical, preserved}, "both concurrent versions must survive")
		waitConverged(t, alice, bob, conflicts[0], 60*time.Second)
	})

	// Model v3: an overwrite of a version whose bytes this peer never
	// fetched must not destroy the only copy. The reader never reads the
	// owner's file, then overwrites it blind. The session base is the held
	// version, not the accepted watermark, so the owner preserves its bytes
	// as a conflict sibling and both survive on both peers.
	t.Run("BlindOverwritePreservesUnfetched", func(t *testing.T) {
		listConflicts := func(p *testPeer) []string {
			var out []string
			for _, name := range listNames(t, p) {
				if strings.Contains(name, "blind.conflict-") {
					out = append(out, strings.TrimSpace(name))
				}
			}
			return out
		}
		require.Equal("OK", bob.send(t, "write_file blind.txt owner-unfetched-version", 10*time.Second))
		WaitForFileOnMount(t, filepath.Join(aliceMount, "blind.txt"), 60*time.Second)
		// No read happens on the reader: the bytes never moved.
		require.Equal("OK", alice.send(t, "write_file blind.txt reader-blind-overwrite", 10*time.Second))

		waitConverged(t, alice, bob, "blind.txt", 60*time.Second)
		WaitForCondition(t, 60*time.Second, 500*time.Millisecond, func() bool {
			return len(listConflicts(alice)) >= 1 && len(listConflicts(bob)) >= 1
		}, "waiting for the blind-overwrite conflict copy on both peers")

		canonical := readFile(t, bob, "blind.txt")
		conflicts := listConflicts(bob)
		require.Len(conflicts, 1, "exactly one conflict copy expected")
		preserved := readFile(t, bob, conflicts[0])
		require.ElementsMatch([]string{"owner-unfetched-version", "reader-blind-overwrite"},
			[]string{canonical, preserved}, "the never-fetched version must survive")
		waitConverged(t, alice, bob, conflicts[0], 60*time.Second)
	})

	// EDIT arrivals carry a base too: a bare truncate crosses a concurrent
	// write. The truncate announces EDIT_FILE with its session base; whichever
	// side loses LWW preserves its version as a conflict sibling. Before the
	// base threading, EDIT arrivals were plain LWW and dropped the loser.
	t.Run("TruncateCrossingConflict", func(t *testing.T) {
		if _, err := exec.LookPath("truncate"); err != nil {
			t.Skip("truncate(1) not available; EDIT-base predicate covered by pkg unit tests")
		}
		listConflicts := func(p *testPeer) []string {
			resp := ex(t, p, ".", "ls", 15*time.Second)
			var out []string
			for _, name := range strings.Split(resp, "\\n") {
				if strings.Contains(name, "tcross.conflict-") {
					out = append(out, strings.TrimSpace(name))
				}
			}
			return out
		}
		require.Equal("OK", bob.send(t, "write_file tcross.txt seed-content-longer-than-four", 10*time.Second))
		WaitForFileOnMount(t, filepath.Join(aliceMount, "tcross.txt"), 60*time.Second)
		waitConverged(t, alice, bob, "tcross.txt", 60*time.Second)

		// Crossing inside the debounce window: bob rewrites, alice bare-truncates.
		require.Equal("OK", bob.send(t, "write_file tcross.txt bob-concurrent-full-rewrite", 10*time.Second))
		ex(t, alice, ".", "truncate -s 4 tcross.txt", 15*time.Second)

		waitConverged(t, alice, bob, "tcross.txt", 60*time.Second)
		WaitForCondition(t, 60*time.Second, 500*time.Millisecond, func() bool {
			return len(listConflicts(alice)) >= 1 && len(listConflicts(bob)) >= 1
		}, "waiting for the crossing conflict copy on both peers")

		canonical := strings.TrimSpace(ex(t, bob, ".", "cat tcross.txt", 15*time.Second))
		conflicts := listConflicts(bob)
		require.Len(conflicts, 1, "exactly one conflict copy expected")
		preserved := strings.TrimSpace(ex(t, bob, ".", "cat "+conflicts[0], 15*time.Second))
		require.ElementsMatch([]string{"bob-concurrent-full-rewrite", "seed"},
			[]string{canonical, preserved}, "both crossing versions must survive")
		waitConverged(t, alice, bob, conflicts[0], 60*time.Second)
	})

	// C.5 metric: a big-file app save (copy, patch one region, swap) must NOT
	// refetch the whole file on the reader — the reconcile path keeps chunks
	// whose xxh3 matches. dlbytes reads the reader's fetched-byte counter.
	t.Run("LargeSwapReconcile", func(t *testing.T) {
		dlbytes := func(p *testPeer, rel string) uint64 {
			resp := p.send(t, "dlbytes "+rel, 5*time.Second)
			require.True(strings.HasPrefix(resp, "DLBYTES:"), "dlbytes failed: %s", resp)
			n, err := strconv.ParseUint(strings.TrimPrefix(resp, "DLBYTES:"), 10, 64)
			require.NoError(err)
			return n
		}
		const big = "doc.big"
		writeRand(t, bob, big, 24*1048576)
		WaitForFileOnMount(t, filepath.Join(aliceMount, big), 60*time.Second)
		waitConverged(t, alice, bob, big, 120*time.Second)
		c1 := dlbytes(alice, big)

		copyFile(t, bob, big, big+".new")
		writeRandAt(t, bob, big+".new", 7*1048576, 1048576)
		renameOver(t, bob, big+".new", big)
		waitConverged(t, alice, bob, big, 120*time.Second)

		c2 := dlbytes(alice, big)
		delta := c2 - c1
		if c2 < c1 { // counter reset by a fresh download cycle
			delta = c2
		}
		t.Logf("swap-save refetch: %d bytes for a 1 MiB change in a 24 MiB file (c1=%d c2=%d)", delta, c1, c2)
		require.Less(delta, uint64(24*1048576), "reader refetched the full file: reconcile never fired")
	})
}

// Editor/tool tier, structured like the git and pijul tests: cold first-contact reads,
// turn-taking handoffs, and tool-generated files (backups, journals, converts) arriving
// fresh on the peer. Each tool saves with a different topology; the oplog records which
// blocks went dirty.
func TestFUSEtoFUSE_EditorSaves(t *testing.T) {
	skipIfNoFUSE(t)

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := newMountPoint(t)
	bobMount := newMountPoint(t)

	logDir := os.Getenv("KD_BIDIR_LOGDIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	peerEnv := func(in, out int, mount string) map[string]string {
		return map[string]string{
			"RELAY_URL":             relay.URL(),
			"INBOUND_PORT":          fmt.Sprintf("%d", in),
			"OUTBOUND_PORT":         fmt.Sprintf("%d", out),
			"MOUNT_DIR":             mount,
			"SAVE_DIR":              t.TempDir(),
			"USE_FUSE":              "1",
			"LOG_FILE":              filepath.Join(logDir, "ed-"+filepath.Base(filepath.Dir(mount))+".log"),
			"KEIBIDROP_LIVE_COLLAB": "1",
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

	ex := func(t *testing.T, p *testPeer, dir, cmdline string, timeout time.Duration) string {
		t.Helper()
		resp := p.send(t, "exec "+dir+" "+cmdline, timeout)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "%q failed: %s", cmdline, resp)
		return strings.TrimPrefix(resp, "EXEC:0:")
	}
	// exSh routes through sh -c on the peer: quotes and parens survive
	// (gimp script-fu batch lines break under exec's space-splitting).
	exSh := func(t *testing.T, p *testPeer, dir, cmdline string, timeout time.Duration) string {
		t.Helper()
		resp := p.send(t, "exec_sh "+dir+" "+cmdline, timeout)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "%q failed: %s", cmdline, resp)
		return strings.TrimPrefix(resp, "EXEC:0:")
	}
	md5cmd := "md5 -q"
	if runtime.GOOS == "linux" {
		md5cmd = "md5sum"
	}
	fileMd5 := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		out := ex(t, p, ".", md5cmd+" "+rel, 60*time.Second)
		return strings.Fields(out)[0]
	}
	// Live sums on both sides, and the empty-file sum never converges: see the
	// pattern-suite helper for why.
	waitConverged := func(t *testing.T, reader, owner *testPeer, rel string, timeout time.Duration) {
		t.Helper()
		const emptyMd5 = "d41d8cd98f00b204e9800998ecf8427e"
		start := time.Now()
		deadline := start.Add(timeout)
		var got, want string
		for time.Now().Before(deadline) {
			want = fileMd5(t, owner, rel)
			got = fileMd5(t, reader, rel)
			if got == want && want != emptyMd5 {
				t.Logf("%s converged in %v", rel, time.Since(start).Round(10*time.Millisecond))
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatalf("%s: no convergence after %v: owner=%s reader=%s", rel, timeout, want, got)
	}

	if _, err := exec.LookPath("vim"); err == nil {
		t.Run("VimTurnTaking", func(t *testing.T) {
			require.Equal("OK", bob.send(t, "write_file note.txt one-two-three", 10*time.Second))
			WaitForFileOnMount(t, filepath.Join(aliceMount, "note.txt"), 60*time.Second)
			waitConverged(t, alice, bob, "note.txt", 30*time.Second)

			// Bob saves via vim: default writebackup does the temp+rename dance.
			ex(t, bob, ".", "vim -es -u NONE -c :%s/one/ONE/g -c :wq note.txt", 30*time.Second)
			waitConverged(t, alice, bob, "note.txt", 30*time.Second)

			// Handoff: alice saves via vim on the same file through her mount.
			ex(t, alice, ".", "vim -es -u NONE -c :%s/three/THREE/g -c :wq note.txt", 30*time.Second)
			waitConverged(t, bob, alice, "note.txt", 60*time.Second)
		})
	}

	if _, err := exec.LookPath("sqlite3"); err == nil {
		t.Run("SqliteJournaled", func(t *testing.T) {
			require.Equal("OK", bob.send(t, "write_file init.sql CREATE TABLE t(x INTEGER); INSERT INTO t VALUES(1),(2),(3);", 10*time.Second))
			ex(t, bob, ".", "sqlite3 -init init.sql data.db .quit", 30*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "data.db"), 60*time.Second)
			waitConverged(t, alice, bob, "data.db", 30*time.Second)

			// In-place page rewrite plus rollback-journal sidecar churn.
			require.Equal("OK", bob.send(t, "write_file upd.sql UPDATE t SET x = x + 40;", 10*time.Second))
			ex(t, bob, ".", "sqlite3 -init upd.sql data.db .quit", 30*time.Second)
			waitConverged(t, alice, bob, "data.db", 60*time.Second)

			// Cold query on the reader: sqlite must see a coherent database, not a torn one.
			require.Equal("OK", alice.send(t, "write_file q.sql SELECT sum(x) FROM t;", 10*time.Second))
			out := ex(t, alice, ".", "sqlite3 -init q.sql data.db .quit", 30*time.Second)
			require.Contains(out, "126", "reader queried a stale or torn database")
		})
	}

	if _, err := exec.LookPath("soffice"); err == nil {
		t.Run("LibreOfficeConvert", func(t *testing.T) {
			require.Equal("OK", bob.send(t, "write_file doc.txt hello-from-keibidrop", 10*time.Second))
			// Fresh zip-container file appears on the owner; reader reads it cold.
			ex(t, bob, ".", "soffice --headless --convert-to odt --outdir . doc.txt", 120*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "doc.odt"), 90*time.Second)
			waitConverged(t, alice, bob, "doc.odt", 60*time.Second)

			// Reader converts the owner's file: cold read + reverse-direction new file.
			ex(t, alice, ".", "soffice --headless --convert-to docx --outdir . doc.odt", 120*time.Second)
			WaitForFileOnMount(t, filepath.Join(bobMount, "doc.docx"), 90*time.Second)
			waitConverged(t, bob, alice, "doc.docx", 60*time.Second)
		})
	}

	if _, err := exec.LookPath("ffmpeg"); err == nil {
		t.Run("FfmpegRenderChain", func(t *testing.T) {
			// Owner renders a clip; reader remuxes it cold through its mount, writing the
			// result back across: the read-edit-handoff chain from the post-prod workflow.
			ex(t, bob, ".", "ffmpeg -hide_banner -loglevel error -y -f lavfi -i testsrc=duration=2:size=320x240:rate=10 clip.mp4", 60*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "clip.mp4"), 60*time.Second)
			waitConverged(t, alice, bob, "clip.mp4", 60*time.Second)

			ex(t, alice, ".", "ffmpeg -hide_banner -loglevel error -y -i clip.mp4 -c copy remux.mp4", 60*time.Second)
			WaitForFileOnMount(t, filepath.Join(bobMount, "remux.mp4"), 60*time.Second)
			waitConverged(t, bob, alice, "remux.mp4", 60*time.Second)
		})
	}

	// GIMP headless: a GUI image tool's real save path (script-fu batch).
	// The reverse leg saves OVER the owner's file — the oplog records which
	// topology file-png-save actually uses.
	gimpBin := ""
	if p, err := exec.LookPath("gimp"); err == nil {
		gimpBin = p
	} else if runtime.GOOS == "darwin" {
		if _, err := os.Stat("/Applications/GIMP.app/Contents/MacOS/gimp"); err == nil {
			gimpBin = "/Applications/GIMP.app/Contents/MacOS/gimp"
		}
	}
	// GIMP 2.x and 3.x have incompatible script-fu APIs (layer-new arg order,
	// file-png-save gone in 3.x, edit-fill renamed, interpreter must be named)
	// and the batch interpreter HANGS on a mismatch instead of erroring.
	// Select the script pair by major version; anything else skips.
	gimpCreate, gimpInvert := "", ""
	if gimpBin != "" {
		out, verr := exec.Command(gimpBin, "--version").CombinedOutput()
		switch {
		case verr == nil && strings.Contains(string(out), "version 2."):
			gimpCreate = gimpBin + ` -i -d -f -b '(let* ((img (car (gimp-image-new 64 64 RGB))) (lay (car (gimp-layer-new img 64 64 RGB-IMAGE "l" 100 LAYER-MODE-NORMAL)))) (gimp-image-insert-layer img lay 0 -1) (gimp-context-set-foreground (list 40 90 200)) (gimp-image-select-rectangle img CHANNEL-OP-REPLACE 8 8 48 48) (gimp-edit-fill lay FILL-FOREGROUND) (gimp-selection-none img) (gimp-image-flatten img) (file-png-save RUN-NONINTERACTIVE img (car (gimp-image-get-active-drawable img)) "art.png" "art" 0 9 1 1 1 1 1) (gimp-quit 0))'`
			gimpInvert = gimpBin + ` -i -d -f -b '(let* ((img (car (gimp-file-load RUN-NONINTERACTIVE "art.png" "art.png")))) (gimp-image-flatten img) (gimp-drawable-invert (car (gimp-image-get-active-drawable img)) FALSE) (file-png-save RUN-NONINTERACTIVE img (car (gimp-image-get-active-drawable img)) "art.png" "art" 0 9 1 1 1 1 1) (gimp-quit 0))'`
		case verr == nil && strings.Contains(string(out), "version 3."):
			// 3.x: gimp-file-save takes (run-mode image file); flatten
			// returns the drawable; load takes no raw-name.
			gimpCreate = gimpBin + ` -i -d -f --batch-interpreter=plug-in-script-fu-eval -b '(let* ((img (car (gimp-image-new 64 64 RGB))) (lay (car (gimp-layer-new img "l" 64 64 RGB-IMAGE 100 LAYER-MODE-NORMAL)))) (gimp-image-insert-layer img lay 0 -1) (gimp-context-set-foreground (list 40 90 200)) (gimp-image-select-rectangle img CHANNEL-OP-REPLACE 8 8 48 48) (gimp-drawable-edit-fill lay FILL-FOREGROUND) (gimp-selection-none img) (gimp-image-flatten img) (gimp-file-save RUN-NONINTERACTIVE img "art.png") (gimp-quit 0))'`
			gimpInvert = gimpBin + ` -i -d -f --batch-interpreter=plug-in-script-fu-eval -b '(let* ((img (car (gimp-file-load RUN-NONINTERACTIVE "art.png"))) (lay (car (gimp-image-flatten img)))) (gimp-drawable-invert lay FALSE) (gimp-file-save RUN-NONINTERACTIVE img "art.png") (gimp-quit 0))'`
		}
	}
	if gimpCreate != "" {
		t.Run("GimpExportOverwrite", func(t *testing.T) {
			exSh(t, bob, ".", gimpCreate, 180*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "art.png"), 60*time.Second)
			waitConverged(t, alice, bob, "art.png", 60*time.Second)

			// Alice inverts the owner's image through her mount and saves over it.
			exSh(t, alice, ".", gimpInvert, 180*time.Second)
			waitConverged(t, bob, alice, "art.png", 60*time.Second)
		})
	}

	// CAD tier: OpenSCAD renders headlessly — source text in the mount, the
	// tool writes a fresh STL over the target (render class). The reverse leg
	// re-renders the OWNER's model from the reader's side.
	scadBin := ""
	if p, err := exec.LookPath("openscad"); err == nil {
		scadBin = p
	} else if runtime.GOOS == "darwin" {
		for _, c := range []string{"/Applications/OpenSCAD.app/Contents/MacOS/OpenSCAD", "/Applications/OpenSCAD-2021.01.app/Contents/MacOS/OpenSCAD"} {
			if _, err := os.Stat(c); err == nil {
				scadBin = c
				break
			}
		}
	}
	if scadBin != "" {
		t.Run("OpenSCADRenderOverwrite", func(t *testing.T) {
			require.Equal("OK", bob.send(t, "write_file model.scad cube([4,4,4]);", 10*time.Second))
			ex(t, bob, ".", scadBin+" -o model.stl model.scad", 180*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "model.stl"), 60*time.Second)
			waitConverged(t, alice, bob, "model.stl", 60*time.Second)

			// Alice edits the source and re-renders over the owner's STL
			// through her own mount.
			require.Equal("OK", alice.send(t, "write_file model.scad sphere(3);", 10*time.Second))
			waitConverged(t, bob, alice, "model.scad", 60*time.Second)
			ex(t, alice, ".", scadBin+" -o model.stl model.scad", 180*time.Second)
			waitConverged(t, bob, alice, "model.stl", 60*time.Second)
		})
	}
}

// Large-asset tier (KD_BIG=1): the bidirectional flows at video-asset scale.
// A 512 MiB seed, then region edits and an app-save swap in both directions;
// dlbytes proves the reader refetches regions, not the file. Skipped by
// default: the run writes ~2 GB and streams the full asset once.
func TestFUSEtoFUSE_BigAssetBidirectional(t *testing.T) {
	skipIfNoFUSE(t)
	if os.Getenv("KD_BIG") == "" {
		t.Skip("large-asset run; set KD_BIG=1")
	}

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := newMountPoint(t)
	bobMount := newMountPoint(t)

	logDir := os.Getenv("KD_BIDIR_LOGDIR")
	if logDir == "" {
		logDir = t.TempDir()
	}
	peerEnv := func(in, out int, mount string) map[string]string {
		return map[string]string{
			"RELAY_URL":             relay.URL(),
			"INBOUND_PORT":          fmt.Sprintf("%d", in),
			"OUTBOUND_PORT":         fmt.Sprintf("%d", out),
			"MOUNT_DIR":             mount,
			"SAVE_DIR":              t.TempDir(),
			"USE_FUSE":              "1",
			"LOG_FILE":              filepath.Join(logDir, filepath.Base(filepath.Dir(mount))+".log"),
			"KEIBIDROP_LIVE_COLLAB": "1",
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

	ex := func(t *testing.T, p *testPeer, dir, cmdline string, timeout time.Duration) string {
		t.Helper()
		resp := p.send(t, "exec "+dir+" "+cmdline, timeout)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "%q failed: %s", cmdline, resp)
		return strings.TrimPrefix(resp, "EXEC:0:")
	}

	md5cmd := "md5 -q"
	if runtime.GOOS == "linux" {
		md5cmd = "md5sum"
	}
	// A full md5 re-reads 512 MiB through the mount each poll; the budget is
	// sized for a local-cache read plus a region refetch, not a full stream.
	fileMd5 := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		out := ex(t, p, ".", md5cmd+" "+rel, 180*time.Second)
		return strings.Fields(out)[0]
	}
	waitConverged := func(t *testing.T, reader, owner *testPeer, rel string, timeout time.Duration) time.Duration {
		t.Helper()
		const emptyMd5 = "d41d8cd98f00b204e9800998ecf8427e"
		start := time.Now()
		deadline := start.Add(timeout)
		var got, want string
		for time.Now().Before(deadline) {
			want = fileMd5(t, owner, rel)
			got = fileMd5(t, reader, rel)
			if got == want && want != emptyMd5 {
				elapsed := time.Since(start)
				t.Logf("converged in %v", elapsed.Round(10*time.Millisecond))
				return elapsed
			}
			time.Sleep(1 * time.Second)
		}
		t.Fatalf("no convergence after %v: owner=%s reader=%s", timeout, want, got)
		return 0
	}
	dlbytes := func(p *testPeer, rel string) uint64 {
		resp := p.send(t, "dlbytes "+rel, 5*time.Second)
		require.True(strings.HasPrefix(resp, "DLBYTES:"), "dlbytes failed: %s", resp)
		n, err := strconv.ParseUint(strings.TrimPrefix(resp, "DLBYTES:"), 10, 64)
		require.NoError(err)
		return n
	}
	// The counter resets on a fresh download cycle; a delta is only meaningful
	// against the same cycle.
	deltaOf := func(c1, c2 uint64) uint64 {
		if c2 < c1 {
			return c2
		}
		return c2 - c1
	}

	const big = "asset.bin"
	const totalMiB = 512
	const totalBytes = uint64(totalMiB) * 1048576

	t.Run("SeedAndFullFetch", func(t *testing.T) {
		ex(t, bob, ".", fmt.Sprintf("dd if=/dev/urandom of=%s bs=1048576 count=%d", big, totalMiB), 600*time.Second)
		WaitForFileOnMount(t, filepath.Join(aliceMount, big), 120*time.Second)
		start := time.Now()
		ex(t, alice, ".", "dd if="+big+" of=/dev/null bs=1048576", 900*time.Second)
		cold := time.Since(start)
		waitConverged(t, alice, bob, big, 600*time.Second)
		c := dlbytes(alice, big)
		t.Logf("cold full read: %d MiB in %v (%.1f MiB/s), fetched %d bytes", totalMiB, cold.Round(10*time.Millisecond), float64(totalMiB)/cold.Seconds(), c)
	})

	// The owner rewrites a 2 MiB region in place; the reader must refetch
	// regions, not the asset.
	t.Run("InPlaceRegionEdit", func(t *testing.T) {
		c1 := dlbytes(alice, big)
		ex(t, bob, ".", "dd if=/dev/urandom of="+big+" bs=1048576 count=2 seek=200 conv=notrunc", 120*time.Second)
		waitConverged(t, alice, bob, big, 600*time.Second)
		delta := deltaOf(c1, dlbytes(alice, big))
		t.Logf("in-place region edit: reader refetched %d bytes for a 2 MiB change", delta)
		require.Less(delta, totalBytes/8, "region reconcile did not fire: near-full refetch")
	})

	// The app-save swap at scale: copy, patch a region, rename over the target.
	t.Run("SwapSaveRegion", func(t *testing.T) {
		c1 := dlbytes(alice, big)
		ex(t, bob, ".", "cp "+big+" "+big+".new", 600*time.Second)
		ex(t, bob, ".", "dd if=/dev/urandom of="+big+".new bs=1048576 count=2 seek=300 conv=notrunc", 120*time.Second)
		ex(t, bob, ".", "mv -f "+big+".new "+big, 30*time.Second)
		waitConverged(t, alice, bob, big, 600*time.Second)
		delta := deltaOf(c1, dlbytes(alice, big))
		t.Logf("swap save: reader refetched %d bytes for a 2 MiB change in %d MiB", delta, totalMiB)
		require.Less(delta, totalBytes/8, "swap lineage did not hold: near-full refetch")
	})

	// Reverse direction: the reader rewrites a region of the owner's asset
	// through its own mount; the owner converges.
	t.Run("ReverseRegionEdit", func(t *testing.T) {
		ex(t, alice, ".", "dd if=/dev/urandom of="+big+" bs=1048576 count=1 seek=100 conv=notrunc", 120*time.Second)
		waitConverged(t, bob, alice, big, 600*time.Second)
	})
}
