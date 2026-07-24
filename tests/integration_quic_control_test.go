// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQUICControlChannel_ConnectsInFlow: peers connect, the QUIC control pair comes up on
// both (keyed from the TCP handshake) and carries a working control Ping.
func TestQUICControlChannel_ConnectsInFlow(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tp.Alice.PingQUICControl(ctx), "alice ping over QUIC control channel")
	require.NoError(t, tp.Bob.PingQUICControl(ctx), "bob ping over QUIC control channel")

	// Metadata rides TCP alongside the QUIC control channel. The QUIC counter is not asserted:
	// a startup TCP transient can legitimately fall the first notify back to QUIC and flake it.
	content := []byte("metadata sync")
	filePath := filepath.Join(tp.AliceSaveDir, "meta.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta.txt", 5*time.Second)
}

// TestQUICControlChannel_FallbackToTCP: with the QUIC lane torn down mid-session, metadata
// keeps flowing over TCP with no stall, the heartbeat demotes the dead lane, and it self-heals.
func TestQUICControlChannel_FallbackToTCP(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	// Kill Alice's QUIC pair so Bob's outbound client now points at a dead server.
	tp.Alice.StopQUICControlChannel()

	// Metadata keeps flowing over TCP both directions, including Bob->Alice whose QUIC server is dead.
	aPath := filepath.Join(tp.AliceSaveDir, "meta-a.txt")
	require.NoError(t, os.WriteFile(aPath, []byte("a"), 0644))
	t0 := time.Now()
	require.NoError(t, tp.Alice.AddFile(aPath))
	require.Less(t, time.Since(t0), 3*time.Second, "metadata must not stall on a dead QUIC lane")
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-a.txt", 5*time.Second)

	bPath := filepath.Join(tp.BobSaveDir, "meta-b.txt")
	require.NoError(t, os.WriteFile(bPath, []byte("b"), 0644))
	t0 = time.Now()
	require.NoError(t, tp.Bob.AddFile(bPath))
	require.Less(t, time.Since(t0), 3*time.Second, "reverse metadata must not stall on a dead QUIC lane")
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "meta-b.txt", 5*time.Second)
	require.Equal(t, uint64(0), tp.Bob.QUICMetadataSent(), "metadata rode TCP, never the dead QUIC lane")

	// Bob's heartbeat detects the dead lane and demotes it.
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return !tp.Bob.QUICControlConnected()
	}, "Bob's control heartbeat should demote the dead QUIC lane")

	// Alice returns; Bob's maintainer re-dials and restores the lane without a reconnect.
	tp.Alice.StartQUICControlChannel()
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.QUICControlConnected() && tp.Alice.QUICControlConnected()
	}, "QUIC channel to self-heal on both peers after Alice returns")
}

// TestQUICControlChannel_Migration: a live QUIC control channel migrates to a fresh local
// UDP socket mid-session (the Wi-Fi to LTE case) and survives with no reconnect or re-handshake.
func TestQUICControlChannel_Migration(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Migrate twice (a device can hop networks repeatedly); the channel must stay connected.
	require.NoError(t, tp.Alice.MigrateQUICControl(ctx), "first migration")
	require.NoError(t, tp.Alice.MigrateQUICControl(ctx), "second migration")
	require.True(t, tp.Alice.QUICControlConnected(), "channel must survive migration (no demote)")

	require.NoError(t, tp.Alice.PingQUICControl(ctx), "control Ping over the migrated QUIC channel")

	content := []byte("after migration")
	filePath := filepath.Join(tp.AliceSaveDir, "meta-migrated.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-migrated.txt", 5*time.Second)
}

// TestQUICControlChannel_LiveFUSE: with a live FUSE mount and the QUIC channel up, a file
// Bob shares is read through Alice's mount; the on-demand Read rides the QUIC interactive lane.
func TestQUICControlChannel_LiveFUSE(t *testing.T) {
	skipIfNoFUSE(t)
	cleanStaleFUSEMounts(t)

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers (FUSE mode)")

	content := []byte("live fuse read over the quic interactive channel")
	bobPath := filepath.Join(tp.BobSaveDir, "quic_fuse.txt")
	require.NoError(t, os.WriteFile(bobPath, content, 0644))
	require.NoError(t, tp.Bob.AddFile(bobPath))

	alicePath := filepath.Join(tp.AliceMountDir, "quic_fuse.txt")
	WaitForFileOnMount(t, alicePath, 15*time.Second)
	data, err := os.ReadFile(alicePath)
	require.NoError(t, err)
	require.Equal(t, content, data, "on-demand read through the live mount must be byte-correct over QUIC")
}
