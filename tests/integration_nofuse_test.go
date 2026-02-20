// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNoFUSE_AddAndPullFile tests the complete no-FUSE file transfer:
// Alice adds a local file -> notifies Bob -> Bob sees it in RemoteFiles ->
// Bob pulls it -> content matches.
func TestNoFUSE_AddAndPullFile(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Create a file in Alice's save directory
	content := []byte("hello from Alice via no-FUSE path")
	filePath := filepath.Join(tp.AliceSaveDir, "test.txt")
	require.NoError(os.WriteFile(filePath, content, 0644))

	// Alice adds the file (notifies Bob via gRPC)
	err := tp.Alice.AddFile(filePath)
	require.NoError(err)

	// Wait for Bob to see it in RemoteFiles
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "test.txt", 5*time.Second)

	// Bob pulls the file
	pullDest := filepath.Join(tp.BobSaveDir, "test.txt")
	err = tp.Bob.PullFile("test.txt", pullDest)
	require.NoError(err)

	// Verify content matches
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content, pulled)
}

// TestNoFUSE_AddMultipleFiles tests adding several files and pulling them all.
func TestNoFUSE_AddMultipleFiles(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	files := map[string][]byte{
		"file1.txt": []byte("content of file 1"),
		"file2.txt": []byte("content of file 2, slightly longer"),
		"file3.bin": make([]byte, 4096),
	}

	// Fill file3 with random data
	rand.Read(files["file3.bin"])

	// Alice adds all files
	for name, content := range files {
		path := filepath.Join(tp.AliceSaveDir, name)
		require.NoError(os.WriteFile(path, content, 0644))
		require.NoError(tp.Alice.AddFile(path))
	}

	// Wait for all files to appear on Bob's side
	for name := range files {
		WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 5*time.Second)
	}

	// Bob pulls all files
	for name, expectedContent := range files {
		pullDest := filepath.Join(tp.BobSaveDir, name)
		require.NoError(tp.Bob.PullFile(name, pullDest))

		pulled, err := os.ReadFile(pullDest)
		require.NoError(err)
		require.Equal(expectedContent, pulled, "content mismatch for %s", name)
	}
}

// TestNoFUSE_LargeFile_1MB tests 1 MB file transfer via gRPC streaming.
func TestNoFUSE_LargeFile_1MB(t *testing.T) {
	testNoFUSELargeFile(t, 1*1024*1024)
}

// TestNoFUSE_LargeFile_5MB tests 5 MB file transfer via gRPC streaming.
func TestNoFUSE_LargeFile_5MB(t *testing.T) {
	testNoFUSELargeFile(t, 5*1024*1024)
}

func testNoFUSELargeFile(t *testing.T, size int) {
	t.Helper()
	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)

	name := fmt.Sprintf("large_%dMB.bin", size/1024/1024)
	data := make([]byte, size)
	rand.Read(data)

	path := filepath.Join(tp.AliceSaveDir, name)
	require.NoError(os.WriteFile(path, data, 0644))
	require.NoError(tp.Alice.AddFile(path))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 10*time.Second)

	pullDest := filepath.Join(tp.BobSaveDir, name)
	require.NoError(tp.Bob.PullFile(name, pullDest))

	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(len(data), len(pulled), "size mismatch")
	require.Equal(data, pulled, "content mismatch")
}

// TestNoFUSE_BidirectionalTransfer tests Alice->Bob and Bob->Alice transfers
// in the same session.
func TestNoFUSE_BidirectionalTransfer(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Alice shares a file
	aliceContent := []byte("from Alice to Bob")
	alicePath := filepath.Join(tp.AliceSaveDir, "alice_file.txt")
	require.NoError(os.WriteFile(alicePath, aliceContent, 0644))
	require.NoError(tp.Alice.AddFile(alicePath))

	// Bob shares a file
	bobContent := []byte("from Bob to Alice")
	bobPath := filepath.Join(tp.BobSaveDir, "bob_file.txt")
	require.NoError(os.WriteFile(bobPath, bobContent, 0644))
	require.NoError(tp.Bob.AddFile(bobPath))

	// Wait for notifications to propagate
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "alice_file.txt", 5*time.Second)
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "bob_file.txt", 5*time.Second)

	// Bob pulls Alice's file
	bobPullDest := filepath.Join(tp.BobSaveDir, "alice_file.txt")
	require.NoError(tp.Bob.PullFile("alice_file.txt", bobPullDest))
	pulled, err := os.ReadFile(bobPullDest)
	require.NoError(err)
	require.Equal(aliceContent, pulled)

	// Alice pulls Bob's file
	alicePullDest := filepath.Join(tp.AliceSaveDir, "bob_file.txt")
	require.NoError(tp.Alice.PullFile("bob_file.txt", alicePullDest))
	pulled, err = os.ReadFile(alicePullDest)
	require.NoError(err)
	require.Equal(bobContent, pulled)
}

// TestNoFUSE_DuplicateAdd tests that adding the same file twice returns os.ErrExist.
func TestNoFUSE_DuplicateAdd(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	path := filepath.Join(tp.AliceSaveDir, "dup.txt")
	require.NoError(os.WriteFile(path, []byte("duplicate test"), 0644))

	// First add should succeed
	require.NoError(tp.Alice.AddFile(path))

	// Second add of the same file should fail
	err := tp.Alice.AddFile(path)
	require.ErrorIs(err, os.ErrExist)
}

// TestNoFUSE_PullNonexistent tests that pulling a file not in RemoteFiles returns ENOENT.
func TestNoFUSE_PullNonexistent(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	pullDest := filepath.Join(tp.BobSaveDir, "nonexistent.txt")
	err := tp.Bob.PullFile("nonexistent.txt", pullDest)
	require.ErrorIs(err, syscall.ENOENT)
}

// TestNoFUSE_ListFiles tests that ListFiles returns correct local and remote entries.
func TestNoFUSE_ListFiles(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Alice adds a file
	path := filepath.Join(tp.AliceSaveDir, "listed.txt")
	require.NoError(os.WriteFile(path, []byte("list me"), 0644))
	require.NoError(tp.Alice.AddFile(path))

	// Wait for Bob to see it
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "listed.txt", 5*time.Second)

	// Alice's local files should include the file
	_, aliceLocal := tp.Alice.ListFiles()
	require.NotEmpty(aliceLocal)

	// Bob's remote files should include the file
	bobRemote, _ := tp.Bob.ListFiles()
	require.NotEmpty(bobRemote)
}
