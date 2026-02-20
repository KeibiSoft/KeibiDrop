// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// testPeer wraps a subprocess running the testpeer binary.
type testPeer struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex // serializes command/response pairs
}

// send writes a command and waits for the response line.
func (p *testPeer) send(t *testing.T, command string, timeout time.Duration) string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()

	_, err := fmt.Fprintln(p.stdin, command)
	if err != nil {
		t.Fatalf("failed to send command %q: %v", command, err)
	}

	done := make(chan string, 1)
	go func() {
		if p.stdout.Scan() {
			done <- p.stdout.Text()
		} else {
			done <- "ERR:scanner closed"
		}
	}()

	select {
	case line := <-done:
		return line
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for response to %q", command)
		return ""
	}
}

// sendAsync sends a command without waiting for a response.
// Returns a channel that will receive the response.
func (p *testPeer) sendAsync(t *testing.T, command string) chan string {
	t.Helper()
	p.mu.Lock()

	_, err := fmt.Fprintln(p.stdin, command)
	if err != nil {
		p.mu.Unlock()
		t.Fatalf("failed to send command %q: %v", command, err)
	}

	ch := make(chan string, 1)
	go func() {
		defer p.mu.Unlock()
		if p.stdout.Scan() {
			ch <- p.stdout.Text()
		} else {
			ch <- "ERR:scanner closed"
		}
	}()
	return ch
}

// buildTestPeer compiles the testpeer binary once per test run.
var (
	testPeerBinary     string
	testPeerBuildOnce  sync.Once
	testPeerBuildError error
)

func getTestPeerBinary(t *testing.T) string {
	t.Helper()
	testPeerBuildOnce.Do(func() {
		binPath := filepath.Join(t.TempDir(), "testpeer")
		cmd := exec.Command("go", "build", "-o", binPath, "./tests/cmd/testpeer/")
		cmd.Dir = filepath.Join("..")
		// Use the project root as working directory
		// Find project root by looking for go.mod
		cwd, _ := os.Getwd()
		projectRoot := cwd
		for {
			if _, err := os.Stat(filepath.Join(projectRoot, "go.mod")); err == nil {
				break
			}
			parent := filepath.Dir(projectRoot)
			if parent == projectRoot {
				testPeerBuildError = fmt.Errorf("could not find go.mod")
				return
			}
			projectRoot = parent
		}
		cmd.Dir = projectRoot
		out, err := cmd.CombinedOutput()
		if err != nil {
			testPeerBuildError = fmt.Errorf("build failed: %v\n%s", err, out)
			return
		}
		testPeerBinary = binPath
	})
	if testPeerBuildError != nil {
		t.Fatalf("testpeer build: %v", testPeerBuildError)
	}
	return testPeerBinary
}

// spawnPeer starts a testpeer subprocess with the given config.
func spawnPeer(t *testing.T, binary string, env map[string]string) *testPeer {
	t.Helper()

	cmd := exec.Command(binary)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Capture stderr for debugging
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)

	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err)

	require.NoError(t, cmd.Start())

	peer := &testPeer{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewScanner(stdoutPipe),
	}

	// Wait for READY
	done := make(chan string, 1)
	go func() {
		if peer.stdout.Scan() {
			done <- peer.stdout.Text()
		} else {
			done <- "ERR:no output"
		}
	}()

	select {
	case line := <-done:
		require.Equal(t, "READY", line, "testpeer should print READY on startup")
	case <-time.After(30 * time.Second):
		cmd.Process.Kill()
		t.Fatal("timeout waiting for testpeer READY")
	}

	return peer
}

// TestFUSEtoFUSE tests two FUSE-enabled peers in separate processes.
// Each peer runs in its own process with its own FUSE mount, communicating
// through a shared mock relay. This tests the path that in-process tests
// cannot: both sides using FUSE simultaneously.
func TestFUSEtoFUSE(t *testing.T) {
	skipIfNoFUSE(t)

	binary := getTestPeerBinary(t)

	// Start mock relay (real HTTP server, accessible from subprocesses).
	relay := NewMockRelay()
	defer relay.Close()

	// Allocate ports (different range from in-process tests to avoid collisions).
	aliceIn := getFreePortInRange(t, 26700, 26749)
	aliceOut := getFreePortInRange(t, 26750, 26799)
	bobIn := getFreePortInRange(t, 26800, 26849)
	bobOut := getFreePortInRange(t, 26850, 26899)

	// Create temp directories.
	aliceMount := t.TempDir()
	aliceSave := t.TempDir()
	bobMount := t.TempDir()
	bobSave := t.TempDir()

	aliceLog := filepath.Join(t.TempDir(), "alice.log")
	bobLog := filepath.Join(t.TempDir(), "bob.log")

	// Spawn Alice (FUSE).
	alice := spawnPeer(t, binary, map[string]string{
		"RELAY_URL":    relay.URL(),
		"INBOUND_PORT": fmt.Sprintf("%d", aliceIn),
		"OUTBOUND_PORT": fmt.Sprintf("%d", aliceOut),
		"MOUNT_DIR":    aliceMount,
		"SAVE_DIR":     aliceSave,
		"USE_FUSE":     "1",
		"LOG_FILE":     aliceLog,
	})

	// Spawn Bob (FUSE).
	bob := spawnPeer(t, binary, map[string]string{
		"RELAY_URL":    relay.URL(),
		"INBOUND_PORT": fmt.Sprintf("%d", bobIn),
		"OUTBOUND_PORT": fmt.Sprintf("%d", bobOut),
		"MOUNT_DIR":    bobMount,
		"SAVE_DIR":     bobSave,
		"USE_FUSE":     "1",
		"LOG_FILE":     bobLog,
	})

	// Cleanup: quit both peers, force unmount, kill processes.
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

	// Exchange fingerprints.
	aliceFP := alice.send(t, "fingerprint", 5*time.Second)
	require.True(strings.HasPrefix(aliceFP, "FP:"), "expected FP: prefix, got: %s", aliceFP)
	aliceFP = strings.TrimPrefix(aliceFP, "FP:")

	bobFP := bob.send(t, "fingerprint", 5*time.Second)
	require.True(strings.HasPrefix(bobFP, "FP:"), "expected FP: prefix, got: %s", bobFP)
	bobFP = strings.TrimPrefix(bobFP, "FP:")

	t.Logf("Alice FP: %s...", aliceFP[:16])
	t.Logf("Bob   FP: %s...", bobFP[:16])

	resp := alice.send(t, "register "+bobFP, 5*time.Second)
	require.Equal("OK", resp, "Alice register peer")

	resp = bob.send(t, "register "+aliceFP, 5*time.Second)
	require.Equal("OK", resp, "Bob register peer")

	// Connect: Alice creates room, Bob joins (concurrently).
	aliceConn := alice.sendAsync(t, "create")

	// Wait for Alice to register on relay before Bob joins.
	WaitForCondition(t, 10*time.Second, 100*time.Millisecond, func() bool {
		return relay.EntryCount() > 0
	}, "waiting for Alice to register on relay")

	bobConn := bob.sendAsync(t, "join")

	select {
	case resp := <-aliceConn:
		require.Equal("CONNECTED", resp, "Alice create room")
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: Alice create room")
	}

	select {
	case resp := <-bobConn:
		require.Equal("CONNECTED", resp, "Bob join room")
	case <-time.After(30 * time.Second):
		t.Fatal("timeout: Bob join room")
	}

	t.Log("Both peers connected via FUSE")

	// Wait for FUSE mounts to be ready.
	waitForFUSEMount(t, aliceMount, 15*time.Second)
	waitForFUSEMount(t, bobMount, 15*time.Second)
	t.Log("Both FUSE mounts ready")

	t.Run("WriteOnAlice_ReadOnBob", func(t *testing.T) {
		// Alice writes a file to her FUSE mount.
		resp := alice.send(t, "write_file hello.txt Hello from Alice FUSE", 10*time.Second)
		require.Equal("OK", resp, "Alice write_file")

		// Bob should see the file on his FUSE mount.
		resp = bob.send(t, "wait_file hello.txt 15", 20*time.Second)
		require.Equal("OK", resp, "Bob wait_file")

		// Bob reads the file from his FUSE mount.
		resp = bob.send(t, "read_file hello.txt", 10*time.Second)
		require.True(strings.HasPrefix(resp, "DATA:"), "expected DATA: prefix, got: %s", resp)
		// Format: DATA:<len>:<content>
		parts := strings.SplitN(resp, ":", 3)
		require.Len(parts, 3)
		require.Equal("Hello from Alice FUSE", parts[2])
	})

	t.Run("WriteOnBob_ReadOnAlice", func(t *testing.T) {
		// Bob writes a file to his FUSE mount.
		resp := bob.send(t, "write_file world.txt Hello from Bob FUSE", 10*time.Second)
		require.Equal("OK", resp, "Bob write_file")

		// Alice should see the file on her FUSE mount.
		resp = alice.send(t, "wait_file world.txt 15", 20*time.Second)
		require.Equal("OK", resp, "Alice wait_file")

		// Alice reads the file from her FUSE mount.
		resp = alice.send(t, "read_file world.txt", 10*time.Second)
		require.True(strings.HasPrefix(resp, "DATA:"), "expected DATA: prefix, got: %s", resp)
		parts := strings.SplitN(resp, ":", 3)
		require.Len(parts, 3)
		require.Equal("Hello from Bob FUSE", parts[2])
	})

	t.Run("BidirectionalSync", func(t *testing.T) {
		// Both peers write files simultaneously.
		resp := alice.send(t, "write_file alice_bidi.txt AliceData", 10*time.Second)
		require.Equal("OK", resp)

		resp = bob.send(t, "write_file bob_bidi.txt BobData", 10*time.Second)
		require.Equal("OK", resp)

		// Each peer should see the other's file.
		resp = bob.send(t, "wait_file alice_bidi.txt 15", 20*time.Second)
		require.Equal("OK", resp, "Bob should see Alice's file")

		resp = alice.send(t, "wait_file bob_bidi.txt 15", 20*time.Second)
		require.Equal("OK", resp, "Alice should see Bob's file")

		// Verify content.
		resp = bob.send(t, "read_file alice_bidi.txt", 10*time.Second)
		parts := strings.SplitN(resp, ":", 3)
		require.Len(parts, 3)
		require.Equal("AliceData", parts[2])

		resp = alice.send(t, "read_file bob_bidi.txt", 10*time.Second)
		parts = strings.SplitN(resp, ":", 3)
		require.Len(parts, 3)
		require.Equal("BobData", parts[2])
	})
}

