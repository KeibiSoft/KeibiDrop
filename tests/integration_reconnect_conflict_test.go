// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Repro for the reconnect conflict gap: both peers edit the same file
// ABOUTME: while the session is down; the re-announce must not silently win.

package tests

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// logContentPresence reports where content exists under roots. Forensics for
// the loss case.
func logContentPresence(t *testing.T, label string, content []byte, roots ...string) {
	t.Helper()
	found := []string{}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || info.Size() > 1<<20 {
				return nil
			}
			b, err := os.ReadFile(path)
			if err == nil && bytes.Contains(b, content) {
				found = append(found, path)
			}
			return nil
		})
	}
	if len(found) == 0 {
		t.Logf("%s: NOT FOUND under any root %v (the bytes are gone)", label, roots)
	} else {
		t.Logf("%s: present at %v", label, found)
	}
}

// TestReconnectConflict_OfflineEditsBothSides: sever the session without a
// goodbye (the mount survives), edit on both sides, reconnect. The older
// side's bytes must survive as a conflict sibling on both peers.
func TestReconnectConflict_OfflineEditsBothSides(t *testing.T) {
	skipIfNoFUSE(t)
	if testing.Short() {
		t.Skip("FUSE reconnect test skipped in short mode")
	}
	require := require.New(t)

	const seedContent = "v1-seed-content"
	tp := SetupFUSEPeerPairPreConnect(t, 180*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			require.NoError(os.WriteFile(filepath.Join(bobSave, "doc.txt"), []byte(seedContent), 0o644))
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	aliceDoc := filepath.Join(tp.AliceMountDir, "doc.txt")
	WaitForFileOnMount(t, aliceDoc, 30*time.Second)

	// Read through the mount so Alice holds the v1 bytes, not just metadata.
	got, err := os.ReadFile(aliceDoc)
	require.NoError(err)
	require.Equal(seedContent, string(got))

	// Sever without a goodbye: Cancel() is what the health monitor does on
	// a dead connection. The mount stays up; the peer just vanishes.
	tp.Alice.StopConnectionResilience()
	tp.Bob.StopConnectionResilience()
	tp.Alice.Cancel()
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Alice.IsRunning()
	}, "waiting for Alice's session to die")
	tp.Bob.Stop()
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Bob.IsRunning()
	}, "waiting for Bob to stop")

	// The mount survived the drop; the file is still served locally.
	_, err = os.Stat(aliceDoc)
	require.NoError(err, "Alice's mount must survive a session drop")

	// Offline edit through the live mount: sets LocalNewer and the base.
	const aliceOffline = "alice-offline-edit"
	require.NoError(os.WriteFile(aliceDoc, []byte(aliceOffline), 0o644))
	// Let the debounced announce fire into the dead session and drop.
	time.Sleep(700 * time.Millisecond)

	// Offline edit on Bob's save dir, stamped newest: LWW direction fixed.
	const bobOffline = "bob-offline-edit-newest"
	bobDoc := filepath.Join(tp.BobSaveDir, "doc.txt")
	require.NoError(os.WriteFile(bobDoc, []byte(bobOffline), 0o644))
	future := time.Now().Add(time.Hour)
	require.NoError(os.Chtimes(bobDoc, future, future))

	// Manual reconnect, the disconnect-suite dance.
	aliceFp, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := tp.Bob.ExportFingerprint()
	require.NoError(err)
	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))
	tp.Relay.Clear()

	aliceReady := testkit.Go(func() error { return reconnectJoin(tp.Alice.CreateRoom) })
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice to re-register on relay")
	bobReady := testkit.Go(func() error { return reconnectJoin(tp.Bob.JoinRoom) })

	testkit.Run(t, func() error {
		return fp.Steps(
			func() error { return testkit.Within(30*time.Second, "Alice CreateRoom round 2", aliceReady) },
			func() error { return testkit.Within(30*time.Second, "Bob JoinRoom round 2", bobReady) },
		)
	})

	// The canonical path converges to the newest offline write (Bob's).
	WaitForCondition(t, 90*time.Second, 500*time.Millisecond, func() bool {
		b, err := os.ReadFile(aliceDoc)
		return err == nil && string(b) == bobOffline
	}, "waiting for the canonical path to converge to the newest offline write")

	// Forensics before the assertion: where are Alice's offline bytes now?
	logContentPresence(t, "alice offline bytes", []byte(aliceOffline),
		tp.AliceSaveDir, tp.BobSaveDir)

	// Alice was provably concurrent: her bytes must survive as a sibling.
	var sibling string
	WaitForCondition(t, 45*time.Second, 500*time.Millisecond, func() bool {
		entries, err := os.ReadDir(tp.AliceMountDir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "doc.conflict-") {
				sibling = e.Name()
				return true
			}
		}
		return false
	}, "waiting for the offline-edit conflict sibling on Alice's mount")

	preserved, err := os.ReadFile(filepath.Join(tp.AliceMountDir, sibling))
	require.NoError(err)
	require.Equal(aliceOffline, string(preserved), "the sibling must hold Alice's offline bytes")

	// The sibling must reach Bob. Tracker keys may carry a leading slash.
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		_, plain := tp.Bob.SyncTracker.RemoteFiles[sibling]
		_, slashed := tp.Bob.SyncTracker.RemoteFiles["/"+sibling]
		return plain || slashed
	}, "waiting for the conflict sibling to reach Bob's tracker")
}
