// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

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

	aliceMount := t.TempDir()
	bobMount := t.TempDir()

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
			"LOG_FILE":      filepath.Join(logDir, filepath.Base(mount)+".log"),
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

	md5cmd := "md5 -q"
	if runtime.GOOS == "linux" {
		md5cmd = "md5sum"
	}
	fileMd5 := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		out := ex(t, p, ".", md5cmd+" "+rel, 30*time.Second)
		return strings.Fields(out)[0]
	}

	// waitConverged polls the reader's md5 through its mount until it matches want.
	// Returns the time it took; fails the subtest on timeout with both sums.
	waitConverged := func(t *testing.T, reader *testPeer, rel, want string, timeout time.Duration) time.Duration {
		t.Helper()
		start := time.Now()
		deadline := start.Add(timeout)
		var got string
		for time.Now().Before(deadline) {
			got = fileMd5(t, reader, rel)
			if got == want {
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
		ex(t, bob, ".", "dd if=/dev/urandom of="+f+" bs=65536 count=8", 30*time.Second)
		want := fileMd5(t, bob, f)
		WaitForFileOnMount(t, filepath.Join(aliceMount, f), 60*time.Second)
		waitConverged(t, alice, f, want, 60*time.Second)
	})

	// The app-save pattern: write a working copy, swap it over the target.
	t.Run("AtomicRenameOverwrite", func(t *testing.T) {
		ex(t, bob, ".", "dd if=/dev/urandom of="+f+".tmp bs=65536 count=8", 30*time.Second)
		ex(t, bob, ".", "mv -f "+f+".tmp "+f, 15*time.Second)
		want := fileMd5(t, bob, f)
		waitConverged(t, alice, f, want, 60*time.Second)
	})

	// Same byte length, content changed: the in-place profile (databases, mmap writers).
	t.Run("InPlaceSameSize", func(t *testing.T) {
		ex(t, bob, ".", "dd if=/dev/urandom of="+f+" bs=4096 count=1 conv=notrunc", 15*time.Second)
		want := fileMd5(t, bob, f)
		waitConverged(t, alice, f, want, 60*time.Second)
	})

	t.Run("TruncateRewrite", func(t *testing.T) {
		ex(t, bob, ".", "dd if=/dev/urandom of="+f+" bs=65536 count=3", 15*time.Second)
		want := fileMd5(t, bob, f)
		waitConverged(t, alice, f, want, 60*time.Second)
	})

	// The reverse direction: the reader edits the owner's file through its mount.
	t.Run("ReverseInPlace", func(t *testing.T) {
		ex(t, alice, ".", "dd if=/dev/urandom of="+f+" bs=4096 count=1 conv=notrunc", 15*time.Second)
		want := fileMd5(t, alice, f)
		waitConverged(t, bob, f, want, 60*time.Second)
	})

	// Rapid save cycles: three rename-overwrites back to back (editors autosaving).
	t.Run("RenameCycleBurst", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			ex(t, bob, ".", "dd if=/dev/urandom of="+f+".tmp bs=65536 count=8", 30*time.Second)
			ex(t, bob, ".", "mv -f "+f+".tmp "+f, 15*time.Second)
		}
		want := fileMd5(t, bob, f)
		waitConverged(t, alice, f, want, 90*time.Second)
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

	aliceMount := t.TempDir()
	bobMount := t.TempDir()

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
			"LOG_FILE":      filepath.Join(logDir, "ed-"+filepath.Base(mount)+".log"),
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
	fileMd5 := func(t *testing.T, p *testPeer, rel string) string {
		t.Helper()
		out := ex(t, p, ".", md5cmd+" "+rel, 60*time.Second)
		return strings.Fields(out)[0]
	}
	waitConverged := func(t *testing.T, reader *testPeer, rel, want string, timeout time.Duration) {
		t.Helper()
		start := time.Now()
		deadline := start.Add(timeout)
		var got string
		for time.Now().Before(deadline) {
			got = fileMd5(t, reader, rel)
			if got == want {
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
			waitConverged(t, alice, "note.txt", fileMd5(t, bob, "note.txt"), 30*time.Second)

			// Bob saves via vim: default writebackup does the temp+rename dance.
			ex(t, bob, ".", "vim -es -u NONE -c :%s/one/ONE/g -c :wq note.txt", 30*time.Second)
			waitConverged(t, alice, "note.txt", fileMd5(t, bob, "note.txt"), 30*time.Second)

			// Handoff: alice saves via vim on the same file through her mount.
			ex(t, alice, ".", "vim -es -u NONE -c :%s/three/THREE/g -c :wq note.txt", 30*time.Second)
			waitConverged(t, bob, "note.txt", fileMd5(t, alice, "note.txt"), 60*time.Second)
		})
	}

	if _, err := exec.LookPath("sqlite3"); err == nil {
		t.Run("SqliteJournaled", func(t *testing.T) {
			require.Equal("OK", bob.send(t, "write_file init.sql CREATE TABLE t(x INTEGER); INSERT INTO t VALUES(1),(2),(3);", 10*time.Second))
			ex(t, bob, ".", "sqlite3 -init init.sql data.db .quit", 30*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "data.db"), 60*time.Second)
			waitConverged(t, alice, "data.db", fileMd5(t, bob, "data.db"), 30*time.Second)

			// In-place page rewrite plus rollback-journal sidecar churn.
			require.Equal("OK", bob.send(t, "write_file upd.sql UPDATE t SET x = x + 40;", 10*time.Second))
			ex(t, bob, ".", "sqlite3 -init upd.sql data.db .quit", 30*time.Second)
			waitConverged(t, alice, "data.db", fileMd5(t, bob, "data.db"), 60*time.Second)

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
			waitConverged(t, alice, "doc.odt", fileMd5(t, bob, "doc.odt"), 60*time.Second)

			// Reader converts the owner's file: cold read + reverse-direction new file.
			ex(t, alice, ".", "soffice --headless --convert-to docx --outdir . doc.odt", 120*time.Second)
			WaitForFileOnMount(t, filepath.Join(bobMount, "doc.docx"), 90*time.Second)
			waitConverged(t, bob, "doc.docx", fileMd5(t, alice, "doc.docx"), 60*time.Second)
		})
	}

	if _, err := exec.LookPath("ffmpeg"); err == nil {
		t.Run("FfmpegRenderChain", func(t *testing.T) {
			// Owner renders a clip; reader remuxes it cold through its mount, writing the
			// result back across: the read-edit-handoff chain from the post-prod workflow.
			ex(t, bob, ".", "ffmpeg -hide_banner -loglevel error -y -f lavfi -i testsrc=duration=2:size=320x240:rate=10 clip.mp4", 60*time.Second)
			WaitForFileOnMount(t, filepath.Join(aliceMount, "clip.mp4"), 60*time.Second)
			waitConverged(t, alice, "clip.mp4", fileMd5(t, bob, "clip.mp4"), 60*time.Second)

			ex(t, alice, ".", "ffmpeg -hide_banner -loglevel error -y -i clip.mp4 -c copy remux.mp4", 60*time.Second)
			WaitForFileOnMount(t, filepath.Join(bobMount, "remux.mp4"), 60*time.Second)
			waitConverged(t, bob, "remux.mp4", fileMd5(t, alice, "remux.mp4"), 60*time.Second)
		})
	}
}
