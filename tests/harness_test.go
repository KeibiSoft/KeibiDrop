// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
)

// TestPair holds two connected KeibiDrop peers for integration testing.
type TestPair struct {
	Alice *common.KeibiDrop
	Bob   *common.KeibiDrop
	Relay *MockRelay

	Cancel context.CancelFunc

	AliceSaveDir  string
	BobSaveDir    string
	AliceMountDir string
	BobMountDir   string

	runWg *sync.WaitGroup // tracks Run() goroutines for clean shutdown
}

// SetupPeerPair creates two KeibiDrop instances connected through a mock relay.
// Uses t.TempDir() for all directories, dynamic ports in the 26100-26999 range,
// and ::1 for IPv6 loopback. Returns a fully connected pair ready for file operations.
func SetupPeerPair(t *testing.T, isFuse bool) *TestPair {
	return SetupPeerPairWithTimeout(t, isFuse, 30*time.Second)
}

// SetupFUSEPeerPair creates a pair where Alice has FUSE and Bob has no-FUSE.
// This avoids the cgofuse limitation where two concurrent mounts in the same
// process race on signal handler registration.
func SetupFUSEPeerPair(t *testing.T, timeout time.Duration) *TestPair {
	return setupPeerPairImpl(t, true, false, timeout)
}

// SetupPeerPairWithTimeout is like SetupPeerPair but with a custom timeout.
func SetupPeerPairWithTimeout(t *testing.T, isFuse bool, timeout time.Duration) *TestPair {
	return setupPeerPairImpl(t, isFuse, isFuse, timeout)
}

// setupPeerPairImpl is the shared implementation for all peer pair constructors.
func setupPeerPairImpl(t *testing.T, aliceFuse bool, bobFuse bool, timeout time.Duration) *TestPair {
	t.Helper()
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	// Start mock relay
	relay := NewMockRelay()

	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	// Create temp dirs (auto-cleaned by testing framework)
	aliceSave := t.TempDir()
	bobSave := t.TempDir()
	aliceMount := t.TempDir()
	bobMount := t.TempDir()

	// Logger — warn level to reduce noise.
	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(handler)

	// Allocate ports in the 26000-27000 range (handshake validates this range)
	aliceInPort := getFreePortInRange(t, 26100, 26249)
	aliceOutPort := getFreePortInRange(t, 26250, 26399)
	bobInPort := getFreePortInRange(t, 26400, 26549)
	bobOutPort := getFreePortInRange(t, 26550, 26699)

	// Create Alice with ::1 loopback
	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		aliceFuse, relayURL, aliceInPort, aliceOutPort,
		aliceMount, aliceSave, true, true, "::1")
	require.NoError(err)

	// Create Bob with ::1 loopback
	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		bobFuse, relayURL, bobInPort, bobOutPort,
		bobMount, bobSave, true, true, "::1")
	require.NoError(err)

	// Exchange fingerprints
	aliceFp, err := kdAlice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := kdBob.ExportFingerprint()
	require.NoError(err)

	require.NoError(kdAlice.AddPeerFingerprint(bobFp))
	require.NoError(kdBob.AddPeerFingerprint(aliceFp))

	// Start Run() loops (tracked via WaitGroup for clean teardown)
	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); kdAlice.Run() }()
	go func() { defer runWg.Done(); kdBob.Run() }()

	// Connect: CreateRoom (Alice) and JoinRoom (Bob) concurrently.
	// CreateRoom registers to relay then waits for inbound handshake.
	// JoinRoom fetches from relay then performs outbound handshake.
	aliceReady := make(chan error, 1)
	bobReady := make(chan error, 1)

	go func() {
		aliceReady <- kdAlice.CreateRoom()
	}()

	// Wait until Alice has registered on the relay (replaces time.Sleep)
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return relay.EntryCount() > 0
	}, "waiting for Alice to register on relay")

	go func() {
		bobReady <- kdBob.JoinRoom()
	}()

	// Wait for both to complete or timeout
	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice CreateRoom failed")
	case <-ctx.Done():
		t.Fatal("timeout waiting for Alice CreateRoom")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom failed")
	case <-ctx.Done():
		t.Fatal("timeout waiting for Bob JoinRoom")
	}

	tp := &TestPair{
		Alice:         kdAlice,
		Bob:           kdBob,
		Relay:         relay,
		Cancel:        cancel,
		AliceSaveDir:  aliceSave,
		BobSaveDir:    bobSave,
		AliceMountDir: aliceMount,
		BobMountDir:   bobMount,
		runWg:         &runWg,
	}

	t.Cleanup(tp.Teardown)
	return tp
}

// Teardown cleanly shuts down both peers, unmounts filesystems, and cancels context.
func (tp *TestPair) Teardown() {
	// Step 1: Clean unmount via KeibiDrop's FS API.
	// This unblocks the blocking Mount() call in the Run() goroutine.
	if tp.Alice != nil && tp.Alice.FS != nil {
		tp.Alice.FS.Unmount()
	}
	if tp.Bob != nil && tp.Bob.FS != nil {
		tp.Bob.FS.Unmount()
	}

	// Step 2: Cancel context (tells Run() to exit after Mount() returns).
	tp.Cancel()

	// Step 3: Wait for Run() goroutines to fully exit.
	// This prevents ghost goroutines from re-mounting after teardown.
	if tp.runWg != nil {
		done := make(chan struct{})
		go func() { tp.runWg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}

	// Step 4: Force unmount as fallback via /sbin/umount -f.
	for _, dir := range []string{tp.AliceMountDir, tp.BobMountDir} {
		if dir != "" {
			exec.Command("/sbin/umount", "-f", dir).Run()
		}
	}

	// Step 5: Wait for macFUSE to fully release /dev/macfuseN devices.
	// Poll until mount points disappear from `mount` output.
	for _, dir := range []string{tp.AliceMountDir, tp.BobMountDir} {
		if dir != "" {
			waitForUnmount(dir, 5*time.Second)
		}
	}

	if tp.Relay != nil {
		tp.Relay.Close()
	}
}

// waitForUnmount polls until the given directory no longer appears as a mount point.
// Handles macOS symlink resolution (/var -> /private/var).
func waitForUnmount(dir string, timeout time.Duration) {
	// Build list of path variants to check in mount output.
	// On macOS, /var is a symlink to /private/var, but `mount` uses /private/var.
	checks := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		checks = append(checks, resolved)
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(dir, "/var/") {
		checks = append(checks, "/private"+dir)
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return
		}
		mounts := string(out)
		found := false
		for _, check := range checks {
			if strings.Contains(mounts, check) {
				found = true
				break
			}
		}
		if !found {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// WaitForCondition polls a condition function until it returns true or the timeout expires.
func WaitForCondition(t *testing.T, timeout time.Duration, interval time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("WaitForCondition timed out: %s", description)
}

// WaitForRemoteFile waits until a file name appears in the SyncTracker.RemoteFiles map.
func WaitForRemoteFile(t *testing.T, st *synctracker.SyncTracker, fileName string, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 100*time.Millisecond, func() bool {
		st.RemoteFilesMu.RLock()
		defer st.RemoteFilesMu.RUnlock()
		_, ok := st.RemoteFiles[fileName]
		return ok
	}, "waiting for remote file: "+fileName)
}

// WaitForFileOnMount polls os.Stat until a file appears at the given path.
func WaitForFileOnMount(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond, func() bool {
		_, err := os.Stat(path)
		return err == nil
	}, "waiting for file on mount: "+path)
}

// WaitForFileAbsent polls until a file no longer exists at the given path.
func WaitForFileAbsent(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond, func() bool {
		_, err := os.Stat(path)
		return os.IsNotExist(err)
	}, "waiting for file removal at: "+path)
}

// getFreePortInRange finds an available TCP6 port within [low, high].
func getFreePortInRange(t *testing.T, low, high int) int {
	t.Helper()
	for port := low; port <= high; port++ {
		ln, err := net.Listen("tcp6", fmt.Sprintf("[::]:%d", port))
		if err != nil {
			continue // port in use
		}
		ln.Close()
		return port
	}
	t.Fatalf("no free port found in range [%d, %d]", low, high)
	return 0
}

// isFUSEPresent checks whether FUSE is available on the current platform.
// Duplicated from cmd/internal/checkfuse (which can't be imported due to Go internal rules).
func isFUSEPresent() bool {
	switch runtime.GOOS {
	case "darwin":
		if exists("/usr/local/lib/libfuse.dylib") || exists("/Library/Filesystems/macfuse.fs") {
			return true
		}
	case "linux":
		if exists("/lib/x86_64-linux-gnu/libfuse.so.2") || exists("/usr/lib/libfuse.so") || exists("/usr/lib/x86_64-linux-gnu/libfuse3.so") {
			return true
		}
	case "windows":
		if exists(`C:\Windows\System32\winfsp-x64.dll`) {
			return true
		}
	}
	return false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
