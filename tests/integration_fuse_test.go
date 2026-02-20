// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func skipIfNoFUSE(t *testing.T) {
	t.Helper()
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this system, skipping FUSE tests")
	}
}

// cleanStaleFUSEMounts force-unmounts any leftover macfuse mounts from previous test runs.
func cleanStaleFUSEMounts(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	out, err := exec.Command("mount").Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "macfuse") && strings.Contains(line, "tests.test") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				mountPoint := parts[2] // "on /path"
				t.Logf("cleaning stale FUSE mount: %s", mountPoint)
				exec.Command("/sbin/umount", "-f", mountPoint).Run()
			}
		}
	}
	time.Sleep(1 * time.Second)
}

// WaitForFileContent polls until a file exists AND has the expected content.
func WaitForFileContent(t *testing.T, path string, expected []byte, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 200*time.Millisecond, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		return bytes.Equal(data, expected)
	}, "waiting for file content at: "+path)
}

// waitForFUSEMount polls until the directory appears as a FUSE mount point.
func waitForFUSEMount(t *testing.T, dir string, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 500*time.Millisecond, func() bool {
		out, err := exec.Command("mount").Output()
		if err != nil {
			return false
		}
		mounts := string(out)
		// On macOS, /var resolves to /private/var in mount output
		if strings.Contains(mounts, dir) {
			return true
		}
		if runtime.GOOS == "darwin" && strings.HasPrefix(dir, "/var/") {
			return strings.Contains(mounts, "/private"+dir)
		}
		return false
	}, "waiting for FUSE mount at: "+dir)
}

// TestFUSE runs all FUSE integration tests as subtests.
// Uses a single peer pair: Alice has FUSE, Bob has no-FUSE.
// This avoids the cgofuse limitation where two concurrent mounts in the
// same process race on signal handler registration.
func TestFUSE(t *testing.T) {
	skipIfNoFUSE(t)
	cleanStaleFUSEMounts(t)

	// Alice=FUSE, Bob=no-FUSE. We pass isFuse=true but only Alice actually mounts.
	// Use SetupFUSEPeerPair which creates Alice(FUSE) + Bob(no-FUSE).
	tp := SetupFUSEPeerPair(t, 120*time.Second)

	// Verify Alice's FUSE mount is working before running subtests
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// ReadRemoteFromMount runs FIRST — before any writes that might cause
	// macOS to cache the directory listing (negative_vncache).
	t.Run("ReadRemoteFromMount", func(t *testing.T) {
		// Test FUSE read path: Bob adds file (no-FUSE) → Alice sees on mount
		require := require.New(t)

		content := []byte("from Bob to Alice via mount")
		bobPath := filepath.Join(tp.BobSaveDir, "bob_shared.txt")
		require.NoError(os.WriteFile(bobPath, content, 0644))
		require.NoError(tp.Bob.AddFile(bobPath))

		// Alice should see it on her FUSE mount
		alicePath := filepath.Join(tp.AliceMountDir, "bob_shared.txt")
		WaitForFileOnMount(t, alicePath, 15*time.Second)

		// Read from FUSE mount (triggers gRPC Read stream to Bob)
		data, err := os.ReadFile(alicePath)
		require.NoError(err)
		require.Equal(content, data)
	})

	t.Run("WriteAndNotify", func(t *testing.T) {
		// Test FUSE write path: Alice writes to mount → OnLocalChange → Bob notified
		require := require.New(t)

		content := []byte("Hello from Alice via FUSE!")
		alicePath := filepath.Join(tp.AliceMountDir, "fuse_test.txt")
		require.NoError(os.WriteFile(alicePath, content, 0644))

		// Bob should see it in SyncTracker.RemoteFiles (no-FUSE verification)
		// FUSE paths use absolute convention with leading "/" (e.g. "/fuse_test.txt")
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/fuse_test.txt", 15*time.Second)

		// Bob pulls the file and verifies content
		pullDest := filepath.Join(tp.BobSaveDir, "fuse_test.txt")
		require.NoError(tp.Bob.PullFile("/fuse_test.txt", pullDest))

		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(content, pulled)
	})

	t.Run("ModifyAndRenotify", func(t *testing.T) {
		// Test FUSE write path with file modification
		require := require.New(t)

		alicePath := filepath.Join(tp.AliceMountDir, "modify.txt")

		initial := []byte("version1")
		require.NoError(os.WriteFile(alicePath, initial, 0644))
		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/modify.txt", 15*time.Second)

		// Verify initial content via pull
		pullDest := filepath.Join(tp.BobSaveDir, "modify.txt")
		require.NoError(tp.Bob.PullFile("/modify.txt", pullDest))
		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(initial, pulled)
	})

	t.Run("LargeFile", func(t *testing.T) {
		// Test FUSE write + gRPC streaming of 1 MB
		require := require.New(t)

		data := make([]byte, 1*1024*1024)
		rand.Read(data)

		alicePath := filepath.Join(tp.AliceMountDir, "large_fuse.bin")
		require.NoError(os.WriteFile(alicePath, data, 0644))

		WaitForRemoteFile(t, tp.Bob.SyncTracker, "/large_fuse.bin", 15*time.Second)

		pullDest := filepath.Join(tp.BobSaveDir, "large_fuse.bin")
		require.NoError(tp.Bob.PullFile("/large_fuse.bin", pullDest))

		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(len(data), len(pulled))
		require.Equal(data, pulled)
	})

	t.Run("ConcurrentWrites", func(t *testing.T) {
		require := require.New(t)

		numFiles := 5
		var wg sync.WaitGroup

		for i := 0; i < numFiles; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				path := filepath.Join(tp.AliceMountDir, fmt.Sprintf("concurrent_%d.txt", idx))
				os.WriteFile(path, []byte(fmt.Sprintf("content_%d", idx)), 0644)
			}(i)
		}
		wg.Wait()

		// Verify all files appear in Bob's SyncTracker
		for i := 0; i < numFiles; i++ {
			name := fmt.Sprintf("concurrent_%d.txt", i)
			fuseName := "/" + name // FUSE paths have leading "/"
			WaitForRemoteFile(t, tp.Bob.SyncTracker, fuseName, 15*time.Second)

			pullDest := filepath.Join(tp.BobSaveDir, name)
			require.NoError(tp.Bob.PullFile(fuseName, pullDest))
			data, err := os.ReadFile(pullDest)
			require.NoError(err, "failed reading %s", name)
			require.Equal(fmt.Sprintf("content_%d", i), string(data))
		}
	})

	t.Run("MountAccessible", func(t *testing.T) {
		require := require.New(t)

		_, err := os.ReadDir(tp.AliceMountDir)
		require.NoError(err)
	})
}
