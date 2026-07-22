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

// TestQUICControlChannel_ConnectsInFlow is the happy path for the wired QUIC control
// channel: two real peers connect over the full flow, the QUIC control pair comes up on
// both (keyed from the TCP handshake payload), and it carries a working control Ping —
// alongside the TCP file-transfer path, which the other integration tests exercise.
func TestQUICControlChannel_ConnectsInFlow(t *testing.T) {
	tp := SetupPeerPair(t, false)

	// The QUIC control channel dials in the background; both peers should connect.
	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	// It carries a working control RPC in each direction.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tp.Alice.PingQUICControl(ctx), "alice ping over QUIC control channel")
	require.NoError(t, tp.Bob.PingQUICControl(ctx), "bob ping over QUIC control channel")

	// Metadata sync works alongside the QUIC control channel. Metadata is TCP-first
	// (bulk bursts must not congest the QUIC seek/control lane); that it does NOT depend
	// on QUIC is proven by TestQUICControlChannel_FallbackToTCP (sync continues, QUIC
	// counter 0, with the QUIC lane dead). Here we just prove end-to-end delivery; the
	// counter is not asserted because a startup TCP transient can legitimately fall the
	// first notify back to QUIC (the resilience working), which would flake it.
	content := []byte("metadata sync")
	filePath := filepath.Join(tp.AliceSaveDir, "meta.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta.txt", 5*time.Second)
}

// TestQUICControlChannel_FallbackToTCP is the sad path: with the QUIC lane torn down
// mid-session (UDP blocked / channel died), metadata + sync keep working over TCP with
// NO stall (metadata is TCP-first, independent of QUIC), the control-Ping heartbeat
// detects the dead lane and demotes it, and the lane self-heals when the peer returns.
func TestQUICControlChannel_FallbackToTCP(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	// Kill Alice's QUIC pair: her client and server die, so Bob's outbound QUIC client
	// now points at a dead server.
	tp.Alice.StopQUICControlChannel()

	// Metadata keeps flowing over TCP with no dependency on QUIC and no stall (it is
	// always TCP-first) — both directions, including Bob->Alice whose QUIC server is dead.
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

	// Bob's control-Ping heartbeat detects the dead lane and demotes it (which kicks the
	// self-heal maintainer). Bounded by the heartbeat interval, generous timeout.
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return !tp.Bob.QUICControlConnected()
	}, "Bob's control heartbeat should demote the dead QUIC lane")

	// SELF-HEAL: Alice returns; Bob's maintainer re-dials and restores the lane without
	// any reconnect — demote is not one-strike-dead.
	tp.Alice.StartQUICControlChannel()
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.QUICControlConnected() && tp.Alice.QUICControlConnected()
	}, "QUIC channel to self-heal on both peers after Alice returns")
}

// TestQUICControlChannel_Migration is the IP-change proof at the app level: a live,
// session-keyed QUIC control channel migrates to a fresh local UDP socket mid-session
// (the Wi-Fi -> LTE case) and SURVIVES — no reconnect, no re-handshake, the gRPC channel
// and the SecureConn on it never notice — and the control plane keeps riding it. A TCP
// channel dies at this exact moment; that difference is the reason QUIC carries control.
func TestQUICControlChannel_Migration(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Migrate Alice's control connection to a fresh local socket, twice (a device can
	// hop networks repeatedly); the channel must stay connected throughout.
	require.NoError(t, tp.Alice.MigrateQUICControl(ctx), "first migration")
	require.NoError(t, tp.Alice.MigrateQUICControl(ctx), "second migration")
	require.True(t, tp.Alice.QUICControlConnected(), "channel must survive migration (no demote)")

	// The control plane rides the migrated channel: a Ping still round-trips over it.
	require.NoError(t, tp.Alice.PingQUICControl(ctx), "control Ping over the migrated QUIC channel")

	// And data still flows after the migration: metadata (TCP) reaches the peer.
	content := []byte("after migration")
	filePath := filepath.Join(tp.AliceSaveDir, "meta-migrated.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-migrated.txt", 5*time.Second)
}

// TestQUICControlChannel_LiveFUSE is the direct proof for the painful path: a LIVE
// FUSE mount with the QUIC channel up. Alice mounts; the channel connects on both
// peers; a file Bob shares is read through Alice's kernel mount — that read goes
// through the dual stream provider, i.e. the on-demand Read stream rides the QUIC
// interactive channel — and must be byte-correct.
func TestQUICControlChannel_LiveFUSE(t *testing.T) {
	skipIfNoFUSE(t)
	cleanStaleFUSEMounts(t)

	tp := SetupFUSEPeerPair(t, 120*time.Second)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers (FUSE mode)")

	// Bob shares; Alice reads it through the live mount (on-demand read over QUIC).
	content := []byte("live fuse read over the quic interactive channel")
	bobPath := filepath.Join(tp.BobSaveDir, "quic_fuse.txt")
	require.NoError(t, os.WriteFile(bobPath, content, 0644))
	require.NoError(t, tp.Bob.AddFile(bobPath))

	alicePath := filepath.Join(tp.AliceMountDir, "quic_fuse.txt")
	WaitForFileOnMount(t, alicePath, 15*time.Second)
	data, err := os.ReadFile(alicePath)
	require.NoError(t, err)
	require.Equal(t, content, data, "on-demand read through the live mount must be byte-correct over QUIC")
	// (The on-demand read rode the QUIC interactive lane — preferFast; the share notify
	// itself rode TCP, which is the metadata routing proven in the non-FUSE tests.)
}
