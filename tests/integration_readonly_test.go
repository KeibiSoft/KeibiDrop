// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: End-to-end read-only tests: a writer peer cannot alter a
// ABOUTME: share_read_only origin, and a mount_read_only mount returns EROFS.

package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestShareReadOnly_WriterCannotAlterOrigin: Bob shares a file with
// share_read_only. Alice reads it through her mount, then tries to write and
// delete it. Bob's disk must stay byte-identical with the original mtime.
func TestShareReadOnly_WriterCannotAlterOrigin(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	content := []byte("read-only origin bytes")
	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			require.NoError(os.WriteFile(filepath.Join(bobSave, "evidence.txt"), content, 0o644))
			bob.ScanSharedOnStart = true
			bob.ShareReadOnly = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	bobPath := filepath.Join(tp.BobSaveDir, "evidence.txt")
	before, err := os.Stat(bobPath)
	require.NoError(err)

	alicePath := filepath.Join(tp.AliceMountDir, "evidence.txt")
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(alicePath)
		return err == nil && info.Size() == int64(len(content))
	}, "waiting for evidence.txt on Alice's mount")

	got, err := os.ReadFile(alicePath)
	require.NoError(err)
	require.Equal(content, got, "read must work on a read-only share")

	// Alice attacks: overwrite, then delete, then create a new file. Local
	// FUSE ops may succeed in her view; the origin must refuse them all.
	_ = os.WriteFile(alicePath, []byte("tampered"), 0o644)
	_ = os.Remove(filepath.Join(tp.AliceMountDir, "evidence.txt"))
	_ = os.WriteFile(filepath.Join(tp.AliceMountDir, "planted.txt"), []byte("plant"), 0o644)

	// Outlive the watcher debounce (200ms) plus the notify round trip.
	time.Sleep(3 * time.Second)

	after, err := os.Stat(bobPath)
	require.NoError(err, "origin file must still exist")
	bobBytes, err := os.ReadFile(bobPath)
	require.NoError(err)
	require.Equal(content, bobBytes, "origin bytes must be unchanged")
	require.Equal(before.ModTime(), after.ModTime(), "origin mtime must be unchanged")

	_, err = os.Stat(filepath.Join(tp.BobSaveDir, "planted.txt"))
	require.True(os.IsNotExist(err), "planted file must not reach the origin")
}

// TestMountReadOnly_EROFSOnRealMount: with mount_read_only on the FUSE side,
// create/write/remove/rename/mkdir on the mount fail, and reads still work.
func TestMountReadOnly_EROFSOnRealMount(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	content := []byte("look but do not touch")
	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			require.NoError(os.WriteFile(filepath.Join(bobSave, "shared.txt"), content, 0o644))
			bob.ScanSharedOnStart = true
			alice.MountReadOnly = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	alicePath := filepath.Join(tp.AliceMountDir, "shared.txt")
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(alicePath)
		return err == nil && info.Size() == int64(len(content))
	}, "waiting for shared.txt on Alice's read-only mount")

	got, err := os.ReadFile(alicePath)
	require.NoError(err, "reads must work on a read-only mount")
	require.Equal(content, got)

	require.Error(os.WriteFile(filepath.Join(tp.AliceMountDir, "new.txt"), []byte("x"), 0o644), "create must fail")
	f, err := os.OpenFile(alicePath, os.O_WRONLY, 0)
	if err == nil {
		f.Close()
		t.Fatal("write-open must fail on a read-only mount")
	}
	require.Error(os.Remove(alicePath), "unlink must fail")
	require.Error(os.Rename(alicePath, filepath.Join(tp.AliceMountDir, "renamed.txt")), "rename must fail")
	require.Error(os.Mkdir(filepath.Join(tp.AliceMountDir, "newdir"), 0o755), "mkdir must fail")

	// The file is still there and intact after the refused ops.
	got, err = os.ReadFile(alicePath)
	require.NoError(err)
	require.Equal(content, got)
}
