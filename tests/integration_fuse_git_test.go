// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestFUSEtoFUSE_GitClone clones a git repo into Bob's FUSE mount, waits for
// files to sync to Alice's FUSE mount, then verifies Alice can run git log,
// git status, and git show. Both peers have FUSE mounts (separate processes).
func TestFUSEtoFUSE_GitClone(t *testing.T) {
	skipIfNoFUSE(t)

	binary := getTestPeerBinary(t)

	relay := NewMockRelay()
	defer relay.Close()

	aliceIn := getFreePortInRange(t, 26700, 26749)
	aliceOut := getFreePortInRange(t, 26750, 26799)
	bobIn := getFreePortInRange(t, 26800, 26849)
	bobOut := getFreePortInRange(t, 26850, 26899)

	aliceMount := t.TempDir()
	aliceSave := t.TempDir()
	bobMount := t.TempDir()
	bobSave := t.TempDir()

	aliceLog := filepath.Join(t.TempDir(), "alice.log")
	bobLog := filepath.Join(t.TempDir(), "bob.log")

	alice := spawnPeer(t, binary, map[string]string{
		"RELAY_URL":     relay.URL(),
		"INBOUND_PORT":  fmt.Sprintf("%d", aliceIn),
		"OUTBOUND_PORT": fmt.Sprintf("%d", aliceOut),
		"MOUNT_DIR":     aliceMount,
		"SAVE_DIR":      aliceSave,
		"USE_FUSE":      "1",
		"LOG_FILE":      aliceLog,
	})

	bob := spawnPeer(t, binary, map[string]string{
		"RELAY_URL":     relay.URL(),
		"INBOUND_PORT":  fmt.Sprintf("%d", bobIn),
		"OUTBOUND_PORT": fmt.Sprintf("%d", bobOut),
		"MOUNT_DIR":     bobMount,
		"SAVE_DIR":      bobSave,
		"USE_FUSE":      "1",
		"LOG_FILE":      bobLog,
	})

	t.Cleanup(func() {
		for _, p := range []*testPeer{alice, bob} {
			fmt.Fprintln(p.stdin, "quit")
			done := make(chan error, 1)
			go func() { done <- p.cmd.Wait() }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				p.cmd.Process.Kill()
				<-done
			}
		}
		for _, dir := range []string{aliceMount, bobMount} {
			exec.Command("/sbin/umount", "-f", dir).Run()
			waitForUnmount(dir, 5*time.Second)
		}
	})

	require := require.New(t)

	// Exchange fingerprints and connect.
	aliceFP := alice.send(t, "fingerprint", 5*time.Second)
	require.True(strings.HasPrefix(aliceFP, "FP:"), "expected FP:, got: %s", aliceFP)
	aliceFP = strings.TrimPrefix(aliceFP, "FP:")

	bobFP := bob.send(t, "fingerprint", 5*time.Second)
	require.True(strings.HasPrefix(bobFP, "FP:"), "expected FP:, got: %s", bobFP)
	bobFP = strings.TrimPrefix(bobFP, "FP:")

	resp := alice.send(t, "register "+bobFP, 5*time.Second)
	require.Equal("OK", resp, "Alice register")

	resp = bob.send(t, "register "+aliceFP, 5*time.Second)
	require.Equal("OK", resp, "Bob register")

	aliceConn := alice.sendAsync(t, "create")

	WaitForCondition(t, 10*time.Second, 100*time.Millisecond, func() bool {
		return relay.EntryCount() > 0
	}, "waiting for relay registration")

	bobResp := bob.send(t, "join", 30*time.Second)
	require.Equal("CONNECTED", bobResp, "Bob join")

	aliceResp := <-aliceConn
	require.Equal("CONNECTED", aliceResp, "Alice create")

	// Wait for both FUSE mounts.
	waitForFUSEMount(t, aliceMount, 15*time.Second)
	waitForFUSEMount(t, bobMount, 15*time.Second)
	t.Log("Both FUSE mounts ready")

	// Bob clones a git repo into his FUSE mount.
	t.Run("CloneOnBob", func(t *testing.T) {
		resp := bob.send(t, "exec . git clone https://github.com/KeibiSoft/go-fp.git go-fp", 60*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "git clone failed: %s", resp)
		t.Log("Bob: git clone completed")
	})

	// Verify Bob's clone is intact.
	t.Run("BobGitLog", func(t *testing.T) {
		resp := bob.send(t, "exec go-fp git log --oneline", 10*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Bob git log failed: %s", resp)
		output := strings.TrimPrefix(resp, "EXEC:0:")
		lines := strings.Split(output, "\\n")
		require.GreaterOrEqual(len(lines), 1, "expected commits")
		t.Logf("Bob git log: %d commits", len(lines))
	})

	// Wait for files to sync to Alice.
	t.Run("AliceSeesRepo", func(t *testing.T) {
		aliceHEAD := filepath.Join(aliceMount, "go-fp", ".git", "HEAD")
		WaitForFileOnMount(t, aliceHEAD, 30*time.Second)
		t.Log("Alice sees .git/HEAD")

		aliceRef := filepath.Join(aliceMount, "go-fp", ".git", "refs", "heads", "main")
		WaitForFileOnMount(t, aliceRef, 30*time.Second)
		t.Log("Alice sees refs/heads/main")

		// Wait a bit for remaining files to sync
		time.Sleep(3 * time.Second)
	})

	// Alice runs git log on her FUSE mount.
	t.Run("AliceGitLog", func(t *testing.T) {
		resp := alice.send(t, "exec go-fp git log --oneline", 15*time.Second)
		t.Logf("Alice git log response: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Alice git log failed: %s", resp)
		output := strings.TrimPrefix(resp, "EXEC:0:")
		lines := strings.Split(output, "\\n")
		require.GreaterOrEqual(len(lines), 1, "expected commits on Alice")
		t.Logf("Alice git log: %d commits", len(lines))
	})

	// Alice runs git status.
	t.Run("AliceGitStatus", func(t *testing.T) {
		resp := alice.send(t, "exec go-fp git status", 15*time.Second)
		t.Logf("Alice git status response: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Alice git status failed: %s", resp)
		output := strings.TrimPrefix(resp, "EXEC:0:")
		require.NotContains(output, "broken", "Alice git status reports broken")
		t.Log("Alice git status works")
	})

	// Alice checks HEAD content via exec (not read_file, to avoid scanner issues).
	t.Run("AliceHEADIntegrity", func(t *testing.T) {
		resp := alice.send(t, "exec go-fp cat .git/HEAD", 10*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "cat HEAD failed: %s", resp)
		head := strings.TrimPrefix(resp, "EXEC:0:")
		require.Equal("ref: refs/heads/main", head, "Alice HEAD has wrong content")
		t.Logf("Alice HEAD: %s", head)
	})

	// Verify a source file exists on Alice by listing it.
	t.Run("FileExists", func(t *testing.T) {
		aliceGoMod := filepath.Join(aliceMount, "go-fp", "go.mod")
		WaitForFileOnMount(t, aliceGoMod, 15*time.Second)
		t.Log("Alice has go.mod")
	})

	// Alice creates a branch, writes a file, commits, switches back to main.
	// Bob should see the branch and be able to checkout + read the file.
	t.Run("AliceCreateBranch", func(t *testing.T) {
		resp := alice.send(t, "exec go-fp git checkout -b test_cross_peer", 10*time.Second)
		t.Logf("Alice checkout -b: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "checkout -b failed: %s", resp)
	})

	t.Run("AliceWriteAndCommit", func(t *testing.T) {
		resp := alice.send(t, "write_file go-fp/cross_peer.txt hello from alice", 10*time.Second)
		require.Equal("OK", resp, "Alice write_file")

		resp = alice.send(t, "exec go-fp git add cross_peer.txt", 10*time.Second)
		t.Logf("Alice git add: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "git add failed: %s", resp)

		resp = alice.send(t, "exec go-fp git -c user.name=Alice -c user.email=alice@test -c commit.gpgsign=false commit -m cross-peer-test", 15*time.Second)
		t.Logf("Alice git commit: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "git commit failed: %s", resp)
	})

	t.Run("AliceBackToMain", func(t *testing.T) {
		resp := alice.send(t, "exec go-fp git checkout main", 10*time.Second)
		t.Logf("Alice checkout main: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "checkout main failed: %s", resp)

		// cross_peer.txt should NOT exist on main
		resp = alice.send(t, "exec go-fp ls cross_peer.txt", 10*time.Second)
		t.Logf("Alice ls cross_peer.txt on main: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:"), "ls failed: %s", resp)
		// exit code should be non-zero (file not found on main)
		require.False(strings.HasPrefix(resp, "EXEC:0:"), "cross_peer.txt should NOT exist on main")
		t.Log("Alice: cross_peer.txt correctly absent on main")
	})

	// Wait for sync to propagate Alice's branch + objects to Bob.
	t.Run("BobWaitForSync", func(t *testing.T) {
		bobRef := filepath.Join(bobMount, "go-fp", ".git", "refs", "heads", "test_cross_peer")
		WaitForFileOnMount(t, bobRef, 30*time.Second)
		t.Log("Bob sees refs/heads/test_cross_peer")
		time.Sleep(3 * time.Second)
	})

	// Bob should NOT see cross_peer.txt on main.
	t.Run("BobMainNoCrossPeerFile", func(t *testing.T) {
		resp := bob.send(t, "exec go-fp git status", 10*time.Second)
		t.Logf("Bob git status: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Bob git status failed: %s", resp)

		resp = bob.send(t, "exec go-fp ls cross_peer.txt", 10*time.Second)
		require.True(strings.HasPrefix(resp, "EXEC:"), "ls failed: %s", resp)
		require.False(strings.HasPrefix(resp, "EXEC:0:"), "cross_peer.txt should NOT exist on Bob's main")
		t.Log("Bob: cross_peer.txt correctly absent on main")
	})

	// Bob checks out Alice's branch and reads the file.
	t.Run("BobCheckoutAliceBranch", func(t *testing.T) {
		resp := bob.send(t, "exec go-fp git checkout test_cross_peer", 15*time.Second)
		t.Logf("Bob checkout test_cross_peer: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Bob checkout failed: %s", resp)

		resp = bob.send(t, "exec go-fp cat cross_peer.txt", 10*time.Second)
		t.Logf("Bob cat cross_peer.txt: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "cross_peer.txt not found: %s", resp)
		content := strings.TrimPrefix(resp, "EXEC:0:")
		require.Equal("hello from alice", content, "file content mismatch")
		t.Log("Bob reads Alice's file on her branch")
	})

	// Bob runs git log on Alice's branch to see the commit.
	t.Run("BobGitLogOnAliceBranch", func(t *testing.T) {
		resp := bob.send(t, "exec go-fp git log --oneline", 10*time.Second)
		t.Logf("Bob git log on test_cross_peer: %s", resp)
		require.True(strings.HasPrefix(resp, "EXEC:0:"), "Bob git log failed: %s", resp)
		output := strings.TrimPrefix(resp, "EXEC:0:")
		require.Contains(output, "cross-peer-test", "Alice's commit not visible on Bob")
		t.Log("Bob sees Alice's commit on her branch")
	})
}
