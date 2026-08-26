// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Delete-vs-edit outside the 1 s buffer: a peer delete whose base is
// ABOUTME: below dirty local authority preserves the edit as a sibling.

package tests

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

func listConflictFiles(t *testing.T, dir, stem string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), stem+".conflict-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestDeleteConflict_EditRacesDelete drives the REMOVE_FILE handler with the
// three base classes: racing edit preserved, turn-taking and legacy plain.
func TestDeleteConflict_EditRacesDelete(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("FUSE pair test skipped in short mode")
	}
	require := require.New(t)

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			require.NoError(os.WriteFile(filepath.Join(bobSave, "doc.txt"), []byte("v1-from-bob"), 0o644))
			require.NoError(os.WriteFile(filepath.Join(bobSave, "clean.txt"), []byte("clean-v1"), 0o644))
			require.NoError(os.WriteFile(filepath.Join(bobSave, "legacy.txt"), []byte("legacy-v1"), 0o644))
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	for _, name := range []string{"doc.txt", "clean.txt", "legacy.txt"} {
		WaitForFileOnMount(t, filepath.Join(tp.AliceMountDir, name), 30*time.Second)
		_, err := os.ReadFile(filepath.Join(tp.AliceMountDir, name))
		require.NoError(err)
	}

	// The seed identity is what a deleter that never saw the edit declares.
	seedInfo, err := os.Stat(filepath.Join(tp.BobSaveDir, "doc.txt"))
	require.NoError(err)
	seedIdentity := seedInfo.ModTime().UnixNano()

	// Alice edits through her mount: dirty local authority above the seed.
	const edited = "alice-edit-must-survive"
	require.NoError(os.WriteFile(filepath.Join(tp.AliceMountDir, "doc.txt"), []byte(edited), 0o644))
	// Let the edit settle past the debounce so no ADD cancels the remove.
	time.Sleep(1500 * time.Millisecond)

	svc := tp.Alice.KDSvc
	require.NotNil(svc)

	// Case 1, the race: seed base against dirty local state.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: "/doc.txt", BaseMtimeNs: seedIdentity,
	})
	require.NoError(err)
	var siblings []string
	WaitForCondition(t, 20*time.Second, 250*time.Millisecond, func() bool {
		siblings = listConflictFiles(t, tp.AliceSaveDir, "doc")
		return len(siblings) == 1
	}, "waiting for the delete-race conflict sibling")
	got, err := os.ReadFile(filepath.Join(tp.AliceSaveDir, siblings[0]))
	require.NoError(err)
	require.Equal(edited, string(got), "the racing edit must survive as the sibling")
	WaitForFileAbsent(t, filepath.Join(tp.AliceMountDir, "doc.txt"), 15*time.Second)

	// Case 2, turn-taking: clean file, current base, plain delete.
	cleanInfo, err := os.Stat(filepath.Join(tp.BobSaveDir, "clean.txt"))
	require.NoError(err)
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: "/clean.txt", BaseMtimeNs: cleanInfo.ModTime().UnixNano(),
	})
	require.NoError(err)
	WaitForFileAbsent(t, filepath.Join(tp.AliceMountDir, "clean.txt"), 15*time.Second)
	require.Empty(listConflictFiles(t, tp.AliceSaveDir, "clean"), "turn-taking delete must not mint a sibling")

	// Case 3, legacy sender: base 0, plain delete even against dirty state.
	require.NoError(os.WriteFile(filepath.Join(tp.AliceMountDir, "legacy.txt"), []byte("legacy-dirty"), 0o644))
	time.Sleep(1500 * time.Millisecond)
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: "/legacy.txt", BaseMtimeNs: 0,
	})
	require.NoError(err)
	WaitForFileAbsent(t, filepath.Join(tp.AliceMountDir, "legacy.txt"), 15*time.Second)
	require.Empty(listConflictFiles(t, tp.AliceSaveDir, "legacy"), "base 0 must keep the legacy plain-delete contract")
}
