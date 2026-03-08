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

// TestKeibiDropFlow is an end-to-end test: Alice creates a file on her FUSE mount
// and Bob reads it from his FUSE mount. Uses a mock relay and dynamic ports so
// the test runs without external dependencies.
func TestKeibiDropFlow(t *testing.T) {
	skipIfNoFUSE(t)

	tp := SetupPeerPairWithTimeout(t, true, 60*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	waitForFUSEMount(t, tp.BobMountDir, 15*time.Second)

	require := require.New(t)

	testString := "Hello secret file sent from Alice"
	alicePath := filepath.Join(tp.AliceMountDir, "ok.txt")

	file, err := os.Create(alicePath)
	require.NoError(err)
	require.NotNil(file)

	_, err = file.Write([]byte(testString))
	require.NoError(err)
	require.NoError(file.Close())

	// Wait for ok.txt to appear on Bob's mount.
	bobPath := filepath.Join(tp.BobMountDir, "ok.txt")
	WaitForFileOnMount(t, bobPath, 15*time.Second)

	data, err := os.ReadFile(bobPath)
	require.NoError(err)
	require.Equal(testString, string(data))
}
