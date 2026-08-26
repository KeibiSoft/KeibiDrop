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

// logContentPresence reports where the given content currently exists under
// the listed roots. Forensics for the loss case, printed before the failing
// assertion so the red run documents what happened to the bytes.
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

// TestReconnectConflict_OfflineEditsBothSides drives the canonical CRDT
// motivating scenario through the shipped stack: seed a file, sever the
// session without a goodbye (the mount survives, matching the resilience
// park path), edit on BOTH sides while disconnected, reconnect, and require
// that the older side's bytes survive as a conflict sibling.
//
// Today this fails at the sibling wait: the reconnect re-announce carries no
// base (notifyRestoredFiles and ScanAndShareSaveDir both omit BaseMtimeNs),
// base 0 means plain LWW by design, and Alice's offline edit is truncated
// away with no copy. The tie/conflict machinery itself is proven live by
// TestFUSEtoFUSE_BidirectionalEditPatterns/ConcurrentEditConflictCopy; the
// missing ingredient here is only the base on the reconnect path.
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

	// Sever the session without a goodbye. Cancel() is what the health
	// monitor does on a dead connection: the Run loop takes the temporary
	// disconnect branch and the FUSE mount stays up with its in-memory
	// state. No NotifyDisconnect: the peer just vanishes.
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

	// Offline edit on Alice, THROUGH the live mount: the FUSE write path
	// sets LocalNewer and snapshots the edit base from the held version.
	const aliceOffline = "alice-offline-edit"
	require.NoError(os.WriteFile(aliceDoc, []byte(aliceOffline), 0o644))
	// Let the debounced announce fire into the dead session and drop.
	time.Sleep(700 * time.Millisecond)

	// Offline edit on Bob, in his save dir (the no-FUSE surface), stamped
	// strictly newest so the LWW direction is deterministic.
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

	// THE gap assertion. Alice was provably concurrent (she never saw Bob's
	// offline write), so her bytes must survive as a conflict sibling.
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

	// The sibling is an ordinary new file and must reach Bob too. FUSE-origin
	// announces carry a leading slash in the path, so a no-FUSE receiver may
	// key the tracker entry either way.
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		tp.Bob.SyncTracker.RemoteFilesMu.RLock()
		defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
		_, plain := tp.Bob.SyncTracker.RemoteFiles[sibling]
		_, slashed := tp.Bob.SyncTracker.RemoteFiles["/"+sibling]
		return plain || slashed
	}, "waiting for the conflict sibling to reach Bob's tracker")
}
