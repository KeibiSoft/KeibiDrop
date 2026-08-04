// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Pijul is a second VCS profile for the mount, and a harder one than git: its pristine state is
// a single mmap'd Sanakirja B-tree (.pijul/pristine/db) rewritten in place, where git is mostly
// immutable content-addressed objects. Two peers take turns recording in one shared repository,
// so every handoff carries an in-place edit of that mmap'd store across the wire. Needs a pijul
// identity, so the peers inherit HOME and SSH_AUTH_SOCK from the environment.
func TestFUSEtoFUSE_Pijul(t *testing.T) {
	skipIfNoFUSE(t)
	if _, err := exec.LookPath("pijul"); err != nil {
		t.Skip("pijul not installed")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("pijul needs an ssh agent to sign changes")
	}

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := t.TempDir()
	bobMount := t.TempDir()

	peerEnv := func(in, out int, mount string) map[string]string {
		return map[string]string{
			"RELAY_URL":     relay.URL(),
			"INBOUND_PORT":  fmt.Sprintf("%d", in),
			"OUTBOUND_PORT": fmt.Sprintf("%d", out),
			"MOUNT_DIR":     mount,
			"SAVE_DIR":      t.TempDir(),
			"USE_FUSE":      "1",
			"LOG_FILE":      filepath.Join(t.TempDir(), "peer.log"),
			// auto_cache: mmap needs the page cache, which then serves stale pages for a
			// peer's in-place edit unless the mount invalidates on open.
			"KEIBIDROP_LIVE_COLLAB": os.Getenv("KEIBIDROP_LIVE_COLLAB"),
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

	// pijulLog is the peer's view of the change log. A corrupt pristine errors here rather
	// than returning an empty log, so the exit code is the integrity check.
	pijulLog := func(t *testing.T, p *testPeer) string {
		t.Helper()
		resp := p.send(t, "exec proj pijul log", 30*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "pijul log failed: %s", resp)
		return strings.TrimPrefix(resp, "EXEC:0:")
	}

	// waitForChange polls until the peer's pristine carries msg: the proof that an in-place
	// edit of the mmap'd store crossed the wire intact.
	waitForChange := func(t *testing.T, p *testPeer, msg string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		var last string
		for time.Now().Before(deadline) {
			if last = pijulLog(t, p); strings.Contains(last, msg) {
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatalf("change %q never reached this peer; log was: %s", msg, last)
	}

	record := func(t *testing.T, p *testPeer, author, msg string) {
		t.Helper()
		resp := p.send(t, "exec proj pijul record -a -m "+msg+" --author "+author, 60*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "pijul record failed: %s", resp)
		t.Logf("%s recorded %s: %s", author, msg, resp)
	}

	t.Run("BobStartsRepo", func(t *testing.T) {
		require.NoError(os.MkdirAll(filepath.Join(bobMount, "proj"), 0o755))
		resp := bob.send(t, "exec proj pijul init", 30*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "pijul init failed: %s", resp)

		require.Equal("OK", bob.send(t, "write_file proj/a.txt line-one", 10*time.Second))
		resp = bob.send(t, "exec proj pijul add a.txt", 20*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "pijul add failed: %s", resp)
		record(t, bob, "bob", "first")
		require.Contains(pijulLog(t, bob), "first")
	})

	t.Run("AliceReceivesRepo", func(t *testing.T) {
		WaitForFileOnMount(t, filepath.Join(aliceMount, "proj", ".pijul", "pristine", "db"), 60*time.Second)
		waitForChange(t, alice, "first", 60*time.Second)

		resp := alice.send(t, "exec proj cat a.txt", 15*time.Second)
		require.Equal("line-one", strings.TrimPrefix(resp, "EXEC:0:"))
	})

	// The handoff reverses: alice edits bob's file and records over the same pristine.
	t.Run("AliceEditsAndRecords", func(t *testing.T) {
		require.Equal("OK", alice.send(t, "write_file proj/a.txt line-one-edited-by-alice", 10*time.Second))
		record(t, alice, "alice", "second")

		log := pijulLog(t, alice)
		require.Contains(log, "second")
		require.Contains(log, "first", "alice's record dropped bob's change")
	})

	t.Run("BobSeesAliceEdit", func(t *testing.T) {
		waitForChange(t, bob, "second", 90*time.Second)

		resp := bob.send(t, "exec proj cat a.txt", 15*time.Second)
		require.Equal("line-one-edited-by-alice", strings.TrimPrefix(resp, "EXEC:0:"),
			"bob has a stale working copy after alice's edit")
	})

	t.Run("BobAddsFile", func(t *testing.T) {
		require.Equal("OK", bob.send(t, "write_file proj/b.txt from-bob-again", 10*time.Second))
		resp := bob.send(t, "exec proj pijul add b.txt", 20*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "pijul add failed: %s", resp)
		record(t, bob, "bob", "third")
	})

	t.Run("AliceSeesFullHistory", func(t *testing.T) {
		waitForChange(t, alice, "third", 90*time.Second)

		log := pijulLog(t, alice)
		for _, msg := range []string{"first", "second", "third"} {
			require.Contains(log, msg, "alice is missing change %q after the round trip", msg)
		}
		resp := alice.send(t, "exec proj cat b.txt", 15*time.Second)
		require.Equal("from-bob-again", strings.TrimPrefix(resp, "EXEC:0:"))
	})

	// Both pristines must agree, and each must still be queryable.
	t.Run("BothAgree", func(t *testing.T) {
		aliceLog, bobLog := pijulLog(t, alice), pijulLog(t, bob)
		for _, msg := range []string{"first", "second", "third"} {
			require.Contains(aliceLog, msg)
			require.Contains(bobLog, msg)
		}
		for name, p := range map[string]*testPeer{"alice": alice, "bob": bob} {
			resp := p.send(t, "exec proj pijul status", 30*time.Second)
			require.True(strings.HasPrefix(resp, "EXEC:0:"), "%s pijul status failed: %s", name, resp)
			t.Logf("%s status: %s", name, resp)
		}
	})
}

// The shared-single-repo pattern above co-writes one mmap'd pristine from both peers. Pijul's
// native model is one repository per user, synchronized by pull; over the mount that is a pure
// on-demand read of the peer's repo. This is the supported profile, mirroring git clone/pull.
func TestFUSEtoFUSE_PijulPull(t *testing.T) {
	skipIfNoFUSE(t)
	if _, err := exec.LookPath("pijul"); err != nil {
		t.Skip("pijul not installed")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("pijul needs an ssh agent to sign changes")
	}

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceMount := t.TempDir()
	bobMount := t.TempDir()

	logDir := os.Getenv("KD_PIJUL_LOGDIR") // set to keep peer logs after the run
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
			// The supported pijul profile: in-place edit visibility needs auto_cache on macOS.
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

	t.Run("BobStartsRepo", func(t *testing.T) {
		require.NoError(os.MkdirAll(filepath.Join(bobMount, "proj"), 0o755))
		ex(t, bob, "proj", "pijul init", 30*time.Second)
		require.Equal("OK", bob.send(t, "write_file proj/a.txt line-one", 10*time.Second))
		ex(t, bob, "proj", "pijul add a.txt", 20*time.Second)
		ex(t, bob, "proj", "pijul record -a -m first --author bob", 60*time.Second)
	})

	t.Run("AliceClonesFromMount", func(t *testing.T) {
		WaitForFileOnMount(t, filepath.Join(aliceMount, "proj", ".pijul", "pristine", "db"), 60*time.Second)
		ex(t, alice, ".", "pijul clone proj proj-alice", 120*time.Second)
		require.Contains(ex(t, alice, "proj-alice", "pijul log", 30*time.Second), "first")
		require.Equal("line-one", ex(t, alice, "proj-alice", "cat a.txt", 15*time.Second))
	})

	t.Run("BobRecordsMore", func(t *testing.T) {
		require.Equal("OK", bob.send(t, "write_file proj/a.txt line-one-and-two", 10*time.Second))
		ex(t, bob, "proj", "pijul record -a -m second --author bob", 60*time.Second)
	})

	t.Run("AlicePullsSecond", func(t *testing.T) {
		deadline := time.Now().Add(90 * time.Second)
		var log string
		for time.Now().Before(deadline) {
			ex(t, alice, "proj-alice", "pijul pull --all ../proj", 60*time.Second)
			if log = ex(t, alice, "proj-alice", "pijul log", 30*time.Second); strings.Contains(log, "second") {
				break
			}
			time.Sleep(2 * time.Second)
		}
		require.Contains(log, "second", "alice never pulled bob's second change")
		require.Equal("line-one-and-two", ex(t, alice, "proj-alice", "cat a.txt", 15*time.Second))
	})

	// The reverse direction: alice records in her repo, bob pulls it through his mount.
	t.Run("BobPullsAliceChange", func(t *testing.T) {
		require.Equal("OK", alice.send(t, "write_file proj-alice/a.txt line-three-by-alice", 10*time.Second))
		ex(t, alice, "proj-alice", "pijul record -a -m third --author alice", 60*time.Second)

		WaitForFileOnMount(t, filepath.Join(bobMount, "proj-alice", ".pijul", "pristine", "db"), 60*time.Second)
		deadline := time.Now().Add(90 * time.Second)
		var log string
		for time.Now().Before(deadline) {
			ex(t, bob, "proj", "pijul pull --all ../proj-alice", 60*time.Second)
			if log = ex(t, bob, "proj", "pijul log", 30*time.Second); strings.Contains(log, "third") {
				break
			}
			time.Sleep(2 * time.Second)
		}
		require.Contains(log, "third", "bob never pulled alice's change")
		require.Equal("line-three-by-alice", ex(t, bob, "proj", "cat a.txt", 15*time.Second))
	})
}
