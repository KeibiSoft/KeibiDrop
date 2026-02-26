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
