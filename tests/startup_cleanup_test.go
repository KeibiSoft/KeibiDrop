// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupSaveDirCleanup(t *testing.T) {
	// 1. Setup a temporary "Save" directory with some stale content
	saveDir := t.TempDir()
	staleFile := filepath.Join(saveDir, "stale_file.txt")
	err := os.WriteFile(staleFile, []byte("stale content"), 0644)
	require.NoError(t, err)

	staleSubDir := filepath.Join(saveDir, "stale_subdir")
	err = os.Mkdir(staleSubDir, 0755)
	require.NoError(t, err)

	staleFileInSubDir := filepath.Join(staleSubDir, "another_stale.txt")
	err = os.WriteFile(staleFileInSubDir, []byte("more stale content"), 0644)
	require.NoError(t, err)

	// Verify they exist
	_, err = os.Stat(staleFile)
	assert.NoError(t, err)
	_, err = os.Stat(staleFileInSubDir)
	assert.NoError(t, err)

	// 2. Initialize KeibiDrop pointing to this save directory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	relayURL, _ := url.Parse("http://localhost:12345")
	mountDir := t.TempDir()

	// Using NewKeibiDropWithIP to avoid probing for global IPv6
	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26101, 26102, mountDir, saveDir, false, false, "::1")
	require.NoError(t, err)
	require.NotNil(t, kd)

	// 3. Verify that the save directory is now empty
	entries, err := os.ReadDir(saveDir)
	require.NoError(t, err)
	assert.Equal(t, 0, len(entries), "Save directory should be empty after startup cleanup")

	// Verify stale files are actually gone
	_, err = os.Stat(staleFile)
	assert.True(t, os.IsNotExist(err), "Stale file should be deleted")
	_, err = os.Stat(staleFileInSubDir)
	assert.True(t, os.IsNotExist(err), "Stale file in subdir should be deleted")

	// 4. Verify that the directory itself still exists (it should be re-created)
	stat, err := os.Stat(saveDir)
	require.NoError(t, err)
	assert.True(t, stat.IsDir())
}

func TestStartupNoSaveDir(t *testing.T) {
	// Verify that passing an empty saveDir doesn't crash or cause errors
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	relayURL, _ := url.Parse("http://localhost:12345")
	mountDir := t.TempDir()

	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26103, 26104, mountDir, "", false, false, "::1")
	require.NoError(t, err)
	require.NotNil(t, kd)
	assert.Equal(t, "", kd.ToSave)
}

func TestStartupSaveDirIsFile(t *testing.T) {
	// 1. Setup a file where the directory should be
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "not_a_dir")
	err := os.WriteFile(filePath, []byte("i am a file"), 0644)
	require.NoError(t, err)

	// 2. Initialize KeibiDrop pointing to this file as save directory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	relayURL, _ := url.Parse("http://localhost:12345")
	mountDir := t.TempDir()

	// os.RemoveAll(filePath) will delete the file.
	// Then os.MkdirAll(filePath) will create a directory.
	// This should succeed because RemoveAll handles files too.
	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26105, 26106, mountDir, filePath, false, false, "::1")
	require.NoError(t, err, "Should handle the case where toSave is a file by removing it and creating a dir")
	require.NotNil(t, kd)

	stat, err := os.Stat(filePath)
	require.NoError(t, err)
	assert.True(t, stat.IsDir(), "Path should now be a directory")
}

func TestStartupSaveDirPermissionError(t *testing.T) {
	// This test might be platform specific or requires root to fail reliably in some ways,
	// but we can simulate a failure by trying to create a directory where we don't have permission.
	// On Linux/macOS, we can try to use a path under a read-only directory.
	
	if os.Getuid() == 0 {
		t.Skip("Skipping permission test as root")
	}

	baseDir := t.TempDir()
	readOnlyDir := filepath.Join(baseDir, "readonly")
	err := os.Mkdir(readOnlyDir, 0555) // Read and execute, no write
	require.NoError(t, err)
	defer os.Chmod(readOnlyDir, 0755) // Cleanup

	saveDir := filepath.Join(readOnlyDir, "should_fail")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	relayURL, _ := url.Parse("http://localhost:12345")
	mountDir := t.TempDir()

	// This should fail at os.MkdirAll
	_, err = common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26107, 26108, mountDir, saveDir, false, false, "::1")
	assert.Error(t, err, "Should fail when directory cannot be created due to permissions")
}
