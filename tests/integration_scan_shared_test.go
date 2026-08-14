// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration test for scan_shared_on_start — a save dir populated
// ABOUTME: BEFORE pairing appears on the peer's mount, byte-exact, junk excluded.

package tests

import (
	"crypto/rand"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestScanSharedOnStart_PopulatedFolderAppears is the sshfs-parity demo:
// Bob (no-FUSE) points his save dir at a folder that already has files and
// sets scan_shared_on_start. After pairing, the whole tree appears in
// Alice's FUSE mount and reads byte-exact, without any per-file add.
func TestScanSharedOnStart_PopulatedFolderAppears(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	big := make([]byte, 512*1024)
	_, err := rand.Read(big)
	require.NoError(err)

	seed := map[string][]byte{
		"report.txt":          []byte("top level file"),
		"data.bin":            big,
		"models/weights.dat":  append([]byte("w"), big[:128*1024]...),
		"models/cfg/net.json": []byte(`{"layers":3}`),
	}

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			for rel, content := range seed {
				p := filepath.Join(bobSave, filepath.FromSlash(rel))
				require.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(os.WriteFile(p, content, 0o644))
			}
			require.NoError(os.WriteFile(filepath.Join(bobSave, ".DS_Store"), []byte("junk"), 0o644))
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	for rel, content := range seed {
		rel, content := rel, content
		mountPath := filepath.Join(tp.AliceMountDir, filepath.FromSlash(rel))
		WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
			info, err := os.Stat(mountPath)
			return err == nil && info.Size() == int64(len(content))
		}, "waiting for "+rel+" to appear in Alice's mount")

		got, err := os.ReadFile(mountPath)
		require.NoError(err, rel)
		require.Equal(sha256.Sum256(content), sha256.Sum256(got), "content mismatch: %s", rel)
	}

	// Internal files never sync.
	_, err = os.Stat(filepath.Join(tp.AliceMountDir, ".DS_Store"))
	require.True(os.IsNotExist(err), ".DS_Store must not appear on the peer")

	// The mount presents the origin's mtime (metadata contract).
	src, err := os.Stat(filepath.Join(tp.BobSaveDir, "report.txt"))
	require.NoError(err)
	dst, err := os.Stat(filepath.Join(tp.AliceMountDir, "report.txt"))
	require.NoError(err)
	require.WithinDuration(src.ModTime(), dst.ModTime(), time.Second,
		"peer mount must show the origin mtime")
}
