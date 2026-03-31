// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestKeibiDropFlow is the original end-to-end smoke test.
// Alice (FUSE) writes a file, Bob (no-FUSE) pulls it.
func TestKeibiDropFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	pair := SetupFUSEPeerPair(t, 30*time.Second)
	defer pair.Teardown()

	waitForFUSEMount(t, pair.AliceMountDir, 15*time.Second)

	// Alice writes a file via FUSE mount.
	testContent := "Hello secret file sent from Alice"
	filePath := filepath.Join(pair.AliceMountDir, "ok.txt")
	err := os.WriteFile(filePath, []byte(testContent), 0644)
	require.NoError(t, err)

	// Wait for notification to propagate to Bob.
	// FUSE paths use leading "/" convention.
	WaitForRemoteFile(t, pair.Bob.SyncTracker, "/ok.txt", 15*time.Second)

	// Bob pulls the file (no-FUSE mode).
	err = pair.Bob.PullFile("/ok.txt", filepath.Join(pair.BobSaveDir, "ok.txt"))
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(pair.BobSaveDir, "ok.txt"))
	require.NoError(t, err)
	require.Equal(t, testContent, string(data))
}
