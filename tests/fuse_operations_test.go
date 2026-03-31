// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestConfig holds shared test configuration
type TestConfig struct {
	RelayURL     string
	AliceSave    string
	BobSave      string
	AliceMount   string
	BobMount     string
	AliceInPort  int
	AliceOutPort int
	BobInPort    int
	BobOutPort   int
}

func setupTestEnvironment(t *testing.T) (*TestConfig, *common.KeibiDrop, *common.KeibiDrop, func()) {
	t.Skip("Skipping: these tests require two FUSE mounts which is not supported in a single process (cgofuse limitation). Use TestFUSEtoFUSE in integration_fuse_test.go instead.")
	require := require.New(t)
	ctx := context.Background()

	// Use in-process mock relay (no external relay needed).
	relay := NewMockRelay()
	relayURL := relay.URL()

	// Dynamic ports from the test range.
	aliceIn := getFreePortInRange(t, 26100, 26249)
	aliceOut := getFreePortInRange(t, 26250, 26399)
	bobIn := getFreePortInRange(t, 26400, 26549)
	bobOut := getFreePortInRange(t, 26550, 26699)

	cfg := &TestConfig{
		RelayURL:     relayURL,
		AliceSave:    t.TempDir(),
		BobSave:      t.TempDir(),
		AliceMount:   t.TempDir(),
		BobMount:     t.TempDir(),
		AliceInPort:  aliceIn,
		AliceOutPort: aliceOut,
		BobInPort:    bobIn,
		BobOutPort:   bobOut,
	}

	parsedURL, err := url.Parse(cfg.RelayURL)
	require.NoError(err)

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	})
	logger := slog.New(handler)

	// Alice = FUSE, Bob = no-FUSE (cgofuse limitation: one mount per process).
	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		true, parsedURL, cfg.AliceInPort, cfg.AliceOutPort, cfg.AliceMount, cfg.AliceSave, true, true, "::1")
	require.NoError(err)

	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, parsedURL, cfg.BobInPort, cfg.BobOutPort, cfg.BobMount, cfg.BobSave, true, true, "::1")
	require.NoError(err)

	// Exchange fingerprints
	aliceFp, err := kdAlice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := kdBob.ExportFingerprint()
	require.NoError(err)

	require.NoError(kdAlice.AddPeerFingerprint(bobFp))
	require.NoError(kdBob.AddPeerFingerprint(aliceFp))

	// Start both
	go kdAlice.Run()
	go kdBob.Run()

	// Create and join room
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		kdAlice.CreateRoom()
	}()

	time.Sleep(500 * time.Millisecond)

	go func() {
		defer wg.Done()
		kdBob.JoinRoom()
	}()

	// Wait for connection to establish
	time.Sleep(3 * time.Second)

	cleanup := func() {
		kdAlice.Stop()
		kdBob.Stop()
		relay.Close()
		time.Sleep(500 * time.Millisecond)
		exec.Command("/sbin/umount", "-f", cfg.AliceMount).Run()
		exec.Command("/sbin/umount", "-f", cfg.BobMount).Run()
	}

	return cfg, kdAlice, kdBob, cleanup
}

// TestBasicFileOperations tests create, read, write, rename, delete
func TestBasicFileOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("CreateAndRead", func(t *testing.T) {
		content := []byte("Hello from Alice!")
		path := filepath.Join(cfg.AliceMount, "test1.txt")

		// Write on Alice
		require.NoError(os.WriteFile(path, content, 0644))

		// Wait for sync
		time.Sleep(2 * time.Second)

		// Read on Bob
		bobPath := filepath.Join(cfg.BobMount, "test1.txt")
		data, err := os.ReadFile(bobPath)
		require.NoError(err)
		require.Equal(content, data)
	})

	t.Run("ModifyFile", func(t *testing.T) {
		path := filepath.Join(cfg.AliceMount, "modify.txt")

		// Initial content
		require.NoError(os.WriteFile(path, []byte("version1"), 0644))
		time.Sleep(2 * time.Second)

		// Modify
		require.NoError(os.WriteFile(path, []byte("version2-longer"), 0644))
		time.Sleep(2 * time.Second)

		// Check on Bob
		data, err := os.ReadFile(filepath.Join(cfg.BobMount, "modify.txt"))
		require.NoError(err)
		require.Equal("version2-longer", string(data))
	})

	t.Run("RenameFile", func(t *testing.T) {
		oldPath := filepath.Join(cfg.AliceMount, "oldname.txt")
		newPath := filepath.Join(cfg.AliceMount, "newname.txt")

		require.NoError(os.WriteFile(oldPath, []byte("rename test"), 0644))
		time.Sleep(2 * time.Second)

		require.NoError(os.Rename(oldPath, newPath))
		time.Sleep(2 * time.Second)

		// Old should not exist on Bob
		_, err := os.Stat(filepath.Join(cfg.BobMount, "oldname.txt"))
		require.True(os.IsNotExist(err))

		// New should exist on Bob
		data, err := os.ReadFile(filepath.Join(cfg.BobMount, "newname.txt"))
		require.NoError(err)
		require.Equal("rename test", string(data))
	})

	t.Run("DeleteFile", func(t *testing.T) {
		path := filepath.Join(cfg.AliceMount, "todelete.txt")

		require.NoError(os.WriteFile(path, []byte("will be deleted"), 0644))
		time.Sleep(2 * time.Second)

		// Verify exists on Bob
		bobPath := filepath.Join(cfg.BobMount, "todelete.txt")
		_, err := os.Stat(bobPath)
		require.NoError(err)

		// Delete on Alice
		require.NoError(os.Remove(path))
		time.Sleep(2 * time.Second)

		// Should not exist on Bob
		_, err = os.Stat(bobPath)
		require.True(os.IsNotExist(err))
	})
}

// TestDirectoryOperations tests mkdir, rmdir, nested directories
func TestDirectoryOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("CreateDirectory", func(t *testing.T) {
		dirPath := filepath.Join(cfg.AliceMount, "newdir")
		require.NoError(os.Mkdir(dirPath, 0755))
		time.Sleep(2 * time.Second)

		// Check on Bob
		info, err := os.Stat(filepath.Join(cfg.BobMount, "newdir"))
		require.NoError(err)
		require.True(info.IsDir())
	})

	t.Run("NestedDirectories", func(t *testing.T) {
		nestedPath := filepath.Join(cfg.AliceMount, "level1", "level2", "level3")
		require.NoError(os.MkdirAll(nestedPath, 0755))

		// Create file in nested dir
		filePath := filepath.Join(nestedPath, "deep.txt")
		require.NoError(os.WriteFile(filePath, []byte("deep content"), 0644))
		time.Sleep(3 * time.Second)

		// Check on Bob
		bobPath := filepath.Join(cfg.BobMount, "level1", "level2", "level3", "deep.txt")
		data, err := os.ReadFile(bobPath)
		require.NoError(err)
		require.Equal("deep content", string(data))
	})

	t.Run("RemoveDirectory", func(t *testing.T) {
		dirPath := filepath.Join(cfg.AliceMount, "toremove")
		require.NoError(os.Mkdir(dirPath, 0755))
		time.Sleep(2 * time.Second)

		require.NoError(os.Remove(dirPath))
		time.Sleep(2 * time.Second)

		_, err := os.Stat(filepath.Join(cfg.BobMount, "toremove"))
		require.True(os.IsNotExist(err))
	})
}

// TestBidirectionalSync tests edits from both sides
func TestBidirectionalSync(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("AliceToBobToAlice", func(t *testing.T) {
		// Alice creates file
		alicePath := filepath.Join(cfg.AliceMount, "bidir.txt")
		require.NoError(os.WriteFile(alicePath, []byte("from alice"), 0644))
		time.Sleep(2 * time.Second)

		// Bob modifies
		bobPath := filepath.Join(cfg.BobMount, "bidir.txt")
		require.NoError(os.WriteFile(bobPath, []byte("modified by bob"), 0644))
		time.Sleep(2 * time.Second)

		// Alice should see Bob's changes
		data, err := os.ReadFile(alicePath)
		require.NoError(err)
		require.Equal("modified by bob", string(data))
	})

	t.Run("SimultaneousFiles", func(t *testing.T) {
		// Alice creates file A
		require.NoError(os.WriteFile(filepath.Join(cfg.AliceMount, "from_alice.txt"), []byte("alice"), 0644))

		// Bob creates file B (almost simultaneously)
		require.NoError(os.WriteFile(filepath.Join(cfg.BobMount, "from_bob.txt"), []byte("bob"), 0644))

		time.Sleep(3 * time.Second)

		// Both should see both files
		_, err := os.Stat(filepath.Join(cfg.AliceMount, "from_bob.txt"))
		require.NoError(err)
		_, err = os.Stat(filepath.Join(cfg.BobMount, "from_alice.txt"))
		require.NoError(err)
	})
}

// TestLargeFiles tests handling of larger files
func TestLargeFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	sizes := []int{
		1 * 1024,        // 1 KB
		100 * 1024,      // 100 KB
		1 * 1024 * 1024, // 1 MB
		// 10 * 1024 * 1024, // 10 MB (uncomment for thorough testing)
	}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("Size_%dKB", size/1024), func(t *testing.T) {
			data := make([]byte, size)
			_, err := rand.Read(data)
			require.NoError(err)

			path := filepath.Join(cfg.AliceMount, fmt.Sprintf("large_%d.bin", size))
			require.NoError(os.WriteFile(path, data, 0644))

			// Wait proportionally to size
			waitTime := 2*time.Second + time.Duration(size/1024/100)*time.Second
			time.Sleep(waitTime)

			// Verify on Bob
			bobPath := filepath.Join(cfg.BobMount, fmt.Sprintf("large_%d.bin", size))
			readData, err := os.ReadFile(bobPath)
			require.NoError(err)
			require.Equal(len(data), len(readData))
			require.Equal(data, readData)
		})
	}
}

// TestConcurrentOperations tests multiple concurrent file operations
func TestConcurrentOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("ConcurrentWrites", func(t *testing.T) {
		var wg sync.WaitGroup
		numFiles := 5

		for i := 0; i < numFiles; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				path := filepath.Join(cfg.AliceMount, fmt.Sprintf("concurrent_%d.txt", idx))
				os.WriteFile(path, []byte(fmt.Sprintf("content_%d", idx)), 0644)
			}(i)
		}

		wg.Wait()
		time.Sleep(5 * time.Second)

		// Verify all on Bob
		for i := 0; i < numFiles; i++ {
			bobPath := filepath.Join(cfg.BobMount, fmt.Sprintf("concurrent_%d.txt", i))
			_, err := os.Stat(bobPath)
			if err != nil {
				// Retry once more
				time.Sleep(2 * time.Second)
				_, err = os.Stat(bobPath)
			}
			require.NoError(err, "File concurrent_%d.txt not found", i)
		}
	})
}

// TestAppendOperations tests appending to files
func TestAppendOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("AppendToFile", func(t *testing.T) {
		path := filepath.Join(cfg.AliceMount, "append.txt")

		// Create initial
		require.NoError(os.WriteFile(path, []byte("line1\n"), 0644))
		time.Sleep(1 * time.Second)

		// Append
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
		require.NoError(err)
		_, err = f.WriteString("line2\n")
		require.NoError(err)
		f.Close()

		time.Sleep(2 * time.Second)

		// Append more
		f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
		require.NoError(err)
		_, err = f.WriteString("line3\n")
		require.NoError(err)
		f.Close()

		time.Sleep(2 * time.Second)

		// Check on Bob
		data, err := os.ReadFile(filepath.Join(cfg.BobMount, "append.txt"))
		require.NoError(err)
		require.Equal("line1\nline2\nline3\n", string(data))
	})
}

// TestSeekOperations tests random access read/write
func TestSeekOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	cfg, _, _, cleanup := setupTestEnvironment(t)
	defer cleanup()
	require := require.New(t)

	t.Run("RandomWrite", func(t *testing.T) {
		path := filepath.Join(cfg.AliceMount, "seek.bin")

		// Create 1KB file
		data := make([]byte, 1024)
		require.NoError(os.WriteFile(path, data, 0644))
		time.Sleep(2 * time.Second)

		// Write at specific offset
		f, err := os.OpenFile(path, os.O_RDWR, 0644)
		require.NoError(err)
		_, err = f.Seek(512, io.SeekStart)
		require.NoError(err)
		_, err = f.Write([]byte("MARKER"))
		require.NoError(err)
		f.Close()

		time.Sleep(2 * time.Second)

		// Read on Bob and verify marker
		bobData, err := os.ReadFile(filepath.Join(cfg.BobMount, "seek.bin"))
		require.NoError(err)
		require.Equal("MARKER", string(bobData[512:518]))
	})
}
