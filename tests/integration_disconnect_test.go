// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDisconnect_NoFUSE_StopAndReconnect verifies that after both peers
// call Stop(), they can CreateRoom/JoinRoom again without "already running".
func TestDisconnect_NoFUSE_StopAndReconnect(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// --- Phase 1: verify initial connection works ---
	content1 := []byte("before disconnect")
	path1 := filepath.Join(tp.AliceSaveDir, "pre.txt")
	require.NoError(os.WriteFile(path1, content1, 0644))
	require.NoError(tp.Alice.AddFile(path1))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "pre.txt", 10*time.Second)

	pullDest := filepath.Join(tp.BobSaveDir, "pre.txt")
	require.NoError(tp.Bob.PullFile("pre.txt", pullDest))
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content1, pulled)

	// --- Phase 2: disconnect both peers ---
	// Alice notifies Bob (triggers Bob's auto-stop), then stops herself.
	tp.Alice.NotifyDisconnect()
	tp.Alice.Stop()

	// Verify both are no longer running (Bob auto-stops via OnDisconnect callback).
	require.False(tp.Alice.IsRunning(), "Alice should not be running after Stop")
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Bob.IsRunning()
	}, "waiting for Bob to auto-stop after peer disconnect")

	// --- Phase 3: reconnect ---
	// New sessions have new keys → exchange new fingerprints.
	aliceFp2, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp2, err := tp.Bob.ExportFingerprint()
	require.NoError(err)

	require.NoError(tp.Alice.AddPeerFingerprint(bobFp2))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp2))

	// Clear stale relay entries.
	tp.Relay.Clear()

	// Reconnect: CreateRoom (Alice) + JoinRoom (Bob).
	aliceReady := make(chan error, 1)
	bobReady := make(chan error, 1)

	go func() { aliceReady <- tp.Alice.CreateRoom() }()

	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice to re-register on relay")

	go func() { bobReady <- tp.Bob.JoinRoom() }()

	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice CreateRoom (round 2) failed")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for Alice CreateRoom round 2")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom (round 2) failed")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for Bob JoinRoom round 2")
	}

	// --- Phase 4: verify reconnected session works ---
	content2 := []byte("after reconnect!")
	path2 := filepath.Join(tp.AliceSaveDir, "post.txt")
	require.NoError(os.WriteFile(path2, content2, 0644))
	require.NoError(tp.Alice.AddFile(path2))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "post.txt", 10*time.Second)

	pullDest2 := filepath.Join(tp.BobSaveDir, "post.txt")
	require.NoError(tp.Bob.PullFile("post.txt", pullDest2))
	pulled2, err := os.ReadFile(pullDest2)
	require.NoError(err)
	require.Equal(content2, pulled2)
}

// TestDisconnect_NoFUSE_OnePeerDisconnects verifies that when only one peer
// calls Stop(), the other peer can detect it and both can reconnect.
func TestDisconnect_NoFUSE_OnePeerDisconnects(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Bob disconnects (simulates clicking Disconnect button).
	// NotifyDisconnect sends DISCONNECT to Alice, which auto-cancels her context.
	tp.Bob.NotifyDisconnect()
	tp.Bob.Stop()

	require.False(tp.Bob.IsRunning(), "Bob should not be running after Stop")

	// Alice auto-stops via OnDisconnect callback (ctx cancellation in Run()).
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Alice.IsRunning()
	}, "waiting for Alice to auto-stop after peer disconnect")

	// Reconnect with fresh fingerprints.
	aliceFp, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := tp.Bob.ExportFingerprint()
	require.NoError(err)

	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))

	tp.Relay.Clear()

	aliceReady := make(chan error, 1)
	bobReady := make(chan error, 1)

	go func() { aliceReady <- tp.Alice.CreateRoom() }()

	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice to re-register on relay")

	go func() { bobReady <- tp.Bob.JoinRoom() }()

	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice CreateRoom failed after one-sided disconnect")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout on Alice CreateRoom after one-sided disconnect")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom failed after one-sided disconnect")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout on Bob JoinRoom after one-sided disconnect")
	}

	// Verify the new session works.
	content := []byte("reconnected after one-sided disconnect")
	path := filepath.Join(tp.BobSaveDir, "onesided.txt")
	require.NoError(os.WriteFile(path, content, 0644))
	require.NoError(tp.Bob.AddFile(path))
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "onesided.txt", 10*time.Second)

	pullDest := filepath.Join(tp.AliceSaveDir, "onesided.txt")
	require.NoError(tp.Alice.PullFile("onesided.txt", pullDest))
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content, pulled)
}

// TestDisconnect_NoFUSE_AutoStopOnPeerDisconnect verifies that when one peer
// sends NotifyDisconnect(), the receiving peer automatically stops (running=false)
// without needing an explicit Stop() call. This is the real-world scenario:
// user clicks Disconnect → peer gets notified → peer auto-cleans up.
func TestDisconnect_NoFUSE_AutoStopOnPeerDisconnect(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Wait for both peers to be marked running (set asynchronously by Run() goroutine).
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return tp.Alice.IsRunning() && tp.Bob.IsRunning()
	}, "waiting for both peers to be running after setup")

	// Bob disconnects (sends DISCONNECT notification to Alice, then stops himself).
	tp.Bob.NotifyDisconnect()
	tp.Bob.Stop()

	require.False(tp.Bob.IsRunning(), "Bob should not be running after Stop")

	// Alice should auto-stop via the OnDisconnect callback (ctx cancellation).
	// Give it a moment for the goroutine to fire and Run() to process ctx.Done().
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Alice.IsRunning()
	}, "waiting for Alice to auto-stop after peer disconnect")

	// Both peers should now be able to reconnect.
	aliceFp, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := tp.Bob.ExportFingerprint()
	require.NoError(err)

	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))

	tp.Relay.Clear()

	aliceReady := make(chan error, 1)
	bobReady := make(chan error, 1)

	go func() { aliceReady <- tp.Alice.CreateRoom() }()

	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice to re-register on relay")

	go func() { bobReady <- tp.Bob.JoinRoom() }()

	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice CreateRoom after auto-stop")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Alice CreateRoom after auto-stop")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom after auto-stop")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Bob JoinRoom after auto-stop")
	}

	// Verify the reconnected session works.
	content := []byte("after auto-stop reconnect")
	path := filepath.Join(tp.BobSaveDir, "autostop.txt")
	require.NoError(os.WriteFile(path, content, 0644))
	require.NoError(tp.Bob.AddFile(path))
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "autostop.txt", 10*time.Second)

	pullDest := filepath.Join(tp.AliceSaveDir, "autostop.txt")
	require.NoError(tp.Alice.PullFile("autostop.txt", pullDest))
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content, pulled)
}

// TestDisconnect_NoFUSE_RoleSwap verifies that after disconnect, peers can
// swap roles: the one who created now joins, and vice versa. Also tests
// create→disconnect→join and join→disconnect→create sequences.
func TestDisconnect_NoFUSE_RoleSwap(t *testing.T) {
	tp := SetupPeerPair(t, false) // Round 1: Alice=create, Bob=join
	require := require.New(t)

	// --- Round 1: verify connection works ---
	content1 := []byte("round1 data")
	path1 := filepath.Join(tp.AliceSaveDir, "r1.txt")
	require.NoError(os.WriteFile(path1, content1, 0644))
	require.NoError(tp.Alice.AddFile(path1))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "r1.txt", 10*time.Second)

	// --- Disconnect: Alice notifies Bob, then stops ---
	tp.Alice.NotifyDisconnect()
	tp.Alice.Stop()
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Bob.IsRunning()
	}, "waiting for Bob to auto-stop")
	require.False(tp.Alice.IsRunning())

	// --- Round 2: SWAP ROLES — Bob creates, Alice joins ---
	aliceFp, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := tp.Bob.ExportFingerprint()
	require.NoError(err)
	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))
	tp.Relay.Clear()

	bobReady := make(chan error, 1)
	aliceReady := make(chan error, 1)

	// Bob creates this time (was joiner in round 1).
	go func() { bobReady <- tp.Bob.CreateRoom() }()
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Bob to register on relay")

	// Alice joins this time (was creator in round 1).
	go func() { aliceReady <- tp.Alice.JoinRoom() }()

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob CreateRoom (round 2, role swap)")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Bob CreateRoom round 2")
	}
	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice JoinRoom (round 2, role swap)")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Alice JoinRoom round 2")
	}

	// Verify swapped session works — Bob sends file to Alice.
	content2 := []byte("round2 swapped roles")
	path2 := filepath.Join(tp.BobSaveDir, "r2.txt")
	require.NoError(os.WriteFile(path2, content2, 0644))
	require.NoError(tp.Bob.AddFile(path2))
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "r2.txt", 10*time.Second)

	pullDest := filepath.Join(tp.AliceSaveDir, "r2.txt")
	require.NoError(tp.Alice.PullFile("r2.txt", pullDest))
	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content2, pulled)

	// --- Disconnect round 2: Bob notifies Alice, then stops ---
	tp.Bob.NotifyDisconnect()
	tp.Bob.Stop()
	WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
		return !tp.Alice.IsRunning()
	}, "waiting for Alice to auto-stop round 2")

	// --- Round 3: Swap back — Alice creates, Bob joins (original roles) ---
	aliceFp, err = tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err = tp.Bob.ExportFingerprint()
	require.NoError(err)
	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))
	tp.Relay.Clear()

	aliceReady2 := make(chan error, 1)
	bobReady2 := make(chan error, 1)

	go func() { aliceReady2 <- tp.Alice.CreateRoom() }()
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice to register on relay round 3")
	go func() { bobReady2 <- tp.Bob.JoinRoom() }()

	select {
	case err := <-aliceReady2:
		require.NoError(err, "Alice CreateRoom round 3")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Alice CreateRoom round 3")
	}
	select {
	case err := <-bobReady2:
		require.NoError(err, "Bob JoinRoom round 3")
	case <-time.After(15 * time.Second):
		t.Fatal("timeout Bob JoinRoom round 3")
	}

	// Verify round 3 works.
	content3 := []byte("round3 back to original")
	path3 := filepath.Join(tp.AliceSaveDir, "r3.txt")
	require.NoError(os.WriteFile(path3, content3, 0644))
	require.NoError(tp.Alice.AddFile(path3))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "r3.txt", 10*time.Second)
}

// TestDisconnect_NoFUSE_MultipleRounds verifies disconnect/reconnect works
// across 3 consecutive rounds without state leaking between sessions.
func TestDisconnect_NoFUSE_MultipleRounds(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	for round := 1; round <= 3; round++ {
		t.Logf("=== Round %d ===", round)

		// Verify connection.
		require.False(tp.Alice.IsRunning() && round > 1,
			"Alice should not be running at start of round %d", round)

		if round > 1 {
			// Reconnect.
			aliceFp, err := tp.Alice.ExportFingerprint()
			require.NoError(err)
			bobFp, err := tp.Bob.ExportFingerprint()
			require.NoError(err)

			require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
			require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))

			tp.Relay.Clear()

			aliceReady := make(chan error, 1)
			bobReady := make(chan error, 1)

			go func() { aliceReady <- tp.Alice.CreateRoom() }()
			WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
				return tp.Relay.EntryCount() > 0
			}, "waiting for relay registration")
			go func() { bobReady <- tp.Bob.JoinRoom() }()

			select {
			case err := <-aliceReady:
				require.NoError(err, "Alice CreateRoom round %d", round)
			case <-time.After(15 * time.Second):
				t.Fatalf("timeout Alice CreateRoom round %d", round)
			}
			select {
			case err := <-bobReady:
				require.NoError(err, "Bob JoinRoom round %d", round)
			case <-time.After(15 * time.Second):
				t.Fatalf("timeout Bob JoinRoom round %d", round)
			}
		}

		// File transfer in this round (unique names to avoid duplicate errors).
		name := fmt.Sprintf("round%d.txt", round)
		fname := filepath.Join(tp.AliceSaveDir, name)
		require.NoError(os.WriteFile(fname, []byte("round data"), 0644))
		require.NoError(tp.Alice.AddFile(fname))
		WaitForRemoteFile(t, tp.Bob.SyncTracker, name, 10*time.Second)

		// Disconnect: Alice notifies Bob (triggers Bob's auto-stop), then stops herself.
		tp.Alice.NotifyDisconnect()
		tp.Alice.Stop()

		require.False(tp.Alice.IsRunning(), "round %d: Alice should not be running", round)
		// Bob auto-stops via OnDisconnect callback.
		WaitForCondition(t, 5*time.Second, 50*time.Millisecond, func() bool {
			return !tp.Bob.IsRunning()
		}, fmt.Sprintf("round %d: waiting for Bob to auto-stop", round))
	}
}

// TestDisconnect_FUSE_StopAndReconnect verifies that a FUSE client can
// disconnect and reconnect without "already running" or a crash.
// NOTE: Run with -timeout 300s — FUSE mount/unmount cycles need extra time.
func TestDisconnect_FUSE_StopAndReconnect(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	if testing.Short() {
		t.Skip("FUSE disconnect test skipped in short mode")
	}

	tp := SetupFUSEPeerPair(t, 120*time.Second) // Alice=FUSE, Bob=no-FUSE
	require := require.New(t)

	// Phase 1: verify initial connection.
	content1 := []byte("fuse before disconnect")
	path1 := filepath.Join(tp.BobSaveDir, "fuse_pre.txt")
	require.NoError(os.WriteFile(path1, content1, 0644))
	require.NoError(tp.Bob.AddFile(path1))

	// Wait for remote file to appear on FUSE mount.
	fusePath := filepath.Join(tp.AliceMountDir, "fuse_pre.txt")
	WaitForFileOnMount(t, fusePath, 10*time.Second)

	// Phase 2: disconnect.
	// Bob notifies Alice (FUSE) → OnDisconnect unmounts FS + cancels ctx.
	tp.Bob.NotifyDisconnect()
	tp.Bob.Stop()

	// Alice (FUSE) auto-stops: OnDisconnect unmounts FS (unblocks Mount in Run),
	// then cancels ctx → Run() cleans up → running=false.
	WaitForCondition(t, 10*time.Second, 200*time.Millisecond, func() bool {
		return !tp.Alice.IsRunning()
	}, "waiting for Alice (FUSE) to auto-stop after peer disconnect")
	require.False(tp.Bob.IsRunning(), "Bob should not be running after Stop")

	// Phase 3: reconnect.
	aliceFp, err := tp.Alice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := tp.Bob.ExportFingerprint()
	require.NoError(err)

	require.NoError(tp.Alice.AddPeerFingerprint(bobFp))
	require.NoError(tp.Bob.AddPeerFingerprint(aliceFp))

	tp.Relay.Clear()

	aliceReady := make(chan error, 1)
	bobReady := make(chan error, 1)

	go func() { aliceReady <- tp.Alice.CreateRoom() }()
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return tp.Relay.EntryCount() > 0
	}, "waiting for Alice FUSE to re-register")
	go func() { bobReady <- tp.Bob.JoinRoom() }()

	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice (FUSE) CreateRoom round 2")
	case <-time.After(20 * time.Second):
		t.Fatal("timeout Alice FUSE CreateRoom round 2")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom round 2")
	case <-time.After(20 * time.Second):
		t.Fatal("timeout Bob JoinRoom round 2")
	}

	// Phase 4: verify FUSE works again.
	content2 := []byte("fuse after reconnect!")
	path2 := filepath.Join(tp.BobSaveDir, "fuse_post.txt")
	require.NoError(os.WriteFile(path2, content2, 0644))
	require.NoError(tp.Bob.AddFile(path2))

	fusePath2 := filepath.Join(tp.AliceMountDir, "fuse_post.txt")
	WaitForFileOnMount(t, fusePath2, 10*time.Second)
}
