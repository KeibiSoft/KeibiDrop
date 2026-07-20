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

	// Metadata routing: an AddFile notify must ride the QUIC channel (counted), and
	// the peer must receive it — i.e. the QUIC server serves the real KeibiService.
	content := []byte("metadata over quic")
	filePath := filepath.Join(tp.AliceSaveDir, "meta-quic.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-quic.txt", 5*time.Second)
	require.GreaterOrEqual(t, tp.Alice.QUICMetadataSent(), uint64(1),
		"the AddFile notify should have ridden the QUIC metadata channel")
}

// TestQUICControlChannel_FallbackToTCP is the sad path: with the QUIC channel torn down
// mid-session (UDP blocked / channel died), metadata falls back to the TCP channel and
// sync keeps working — the QUIC channel can never lose metadata or break the session.
func TestQUICControlChannel_FallbackToTCP(t *testing.T) {
	tp := SetupPeerPair(t, false)

	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	// Kill Alice's QUIC pair: her client is gone AND her server side dies, so Bob's
	// outbound QUIC client breaks too — both directions must degrade to TCP.
	t0 := time.Now()
	tp.Alice.StopQUICControlChannel()
	t.Logf("stop QUIC channel: %v", time.Since(t0))

	content := []byte("metadata over tcp fallback")
	filePath := filepath.Join(tp.AliceSaveDir, "meta-tcp.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	t0 = time.Now()
	require.NoError(t, tp.Alice.AddFile(filePath))
	t.Logf("alice AddFile (no QUIC client, straight TCP): %v", time.Since(t0))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-tcp.txt", 5*time.Second)

	// And the reverse direction: Bob's QUIC client now points at a dead server. The
	// QUIC-first attempt is bounded by quicMetaTimeout (2s), then falls back to TCP
	// and demotes the channel — without the bound this stalled ~20s (the QUIC idle
	// timeout), measured before the fix.
	bobFile := filepath.Join(tp.BobSaveDir, "meta-tcp-bob.txt")
	require.NoError(t, os.WriteFile(bobFile, content, 0644))
	t0 = time.Now()
	require.NoError(t, tp.Bob.AddFile(bobFile))
	firstSend := time.Since(t0)
	t.Logf("bob AddFile (dead QUIC conn, bounded fallback to TCP): %v", firstSend)
	require.Less(t, firstSend, 4*time.Second, "fallback must be bounded by quicMetaTimeout, not the QUIC idle timeout")
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "meta-tcp-bob.txt", 10*time.Second)

	// After the failed attempt Bob demoted his QUIC client, so the next send skips
	// the dead channel entirely and goes straight to TCP.
	require.False(t, tp.Bob.QUICControlConnected(), "failed QUIC attempt should demote the channel")
	bobFile2 := filepath.Join(tp.BobSaveDir, "meta-tcp-bob2.txt")
	require.NoError(t, os.WriteFile(bobFile2, content, 0644))
	t0 = time.Now()
	require.NoError(t, tp.Bob.AddFile(bobFile2))
	secondSend := time.Since(t0)
	t.Logf("bob AddFile (demoted, straight TCP): %v", secondSend)
	require.Less(t, secondSend, 1*time.Second, "post-demotion sends must go straight to TCP")
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "meta-tcp-bob2.txt", 10*time.Second)

	// SELF-HEAL: the demote was a transient (Alice comes back). Bob's background
	// redial must restore the fast lane without any reconnect — the channel demotes
	// on failure but is not one-strike-dead.
	tp.Alice.StartQUICControlChannel()
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.QUICControlConnected() && tp.Alice.QUICControlConnected()
	}, "QUIC channel to self-heal on both peers after Alice returns")

	// And metadata rides QUIC again.
	before := tp.Bob.QUICMetadataSent()
	bobFile3 := filepath.Join(tp.BobSaveDir, "meta-quic-again.txt")
	require.NoError(t, os.WriteFile(bobFile3, content, 0644))
	require.NoError(t, tp.Bob.AddFile(bobFile3))
	WaitForRemoteFile(t, tp.Alice.SyncTracker, "meta-quic-again.txt", 10*time.Second)
	require.Greater(t, tp.Bob.QUICMetadataSent(), before, "post-heal metadata should ride QUIC again")
}

// TestQUICControlChannel_Migration is the IP-change proof at the app level: a live,
// session-keyed QUIC control channel migrates to a fresh local UDP socket mid-session
// (the Wi-Fi -> LTE case) and SURVIVES — no reconnect, no re-handshake, the gRPC
// channel and the SecureConn on it never notice — and metadata keeps riding it.
// A TCP channel dies at this exact moment; that difference is the reason QUIC carries
// control.
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

	// Metadata still rides the migrated channel.
	before := tp.Alice.QUICMetadataSent()
	content := []byte("metadata over the migrated path")
	filePath := filepath.Join(tp.AliceSaveDir, "meta-migrated.txt")
	require.NoError(t, os.WriteFile(filePath, content, 0644))
	require.NoError(t, tp.Alice.AddFile(filePath))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "meta-migrated.txt", 5*time.Second)
	require.Greater(t, tp.Alice.QUICMetadataSent(), before, "metadata should ride the migrated QUIC channel")
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

	// Bob's share notify rode the QUIC metadata path.
	require.GreaterOrEqual(t, tp.Bob.QUICMetadataSent(), uint64(1), "the share notify should have ridden QUIC")
}
