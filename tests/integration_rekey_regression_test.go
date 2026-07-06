// ABOUTME: Regression tests proving one proactive forward-secrecy rotation does not break
// ABOUTME: real-app file scenarios: rename, remove, nested-dir delete, big + multi-file transfers.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
)

// forceOneRotation drives exactly one proactive rekey on the initiator at an idle
// point and blocks until both peers have re-handshaked. It asserts the responder's
// probe is a guarded no-op (only the deterministic initiator rotates), then probes
// the initiator until it actually rotates. The retry matters because OnRekeyNeeded
// returns false while a REMOVE/RENAME/ADD_DIR notify is still queued (the #6 guard),
// so a directory-creating scenario must let the notify queue drain first. A false
// return has no side effects (it bails before touching LastRekeyAt), so probing
// repeatedly is safe; the first true both starts and commits the single rotation.
func forceOneRotation(t *testing.T, tp *TestPair, initiator, responder *common.KeibiDrop, w *reconnectWatcher, timeout time.Duration) {
	t.Helper()
	require.False(t, responder.HealthMonitor.OnRekeyNeeded(), "responder (non-initiator) must not rotate")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return initiator.HealthMonitor.OnRekeyNeeded()
	}, "initiator to initiate the one forced rotation")
	w.awaitBoth(t, tp, timeout)
}

// notifyWithRetry sends a raw REMOVE/RENAME notification from a peer, retrying until
// it succeeds. Right after a rotation the peer's fresh gRPC client (and the far
// server) are still being rebuilt, so the first call may transiently fail or the
// client may be briefly nil; the retry gives a clean settle without a blind sleep.
// The far handler is idempotent for these ops, so a retried delivery is harmless.
func notifyWithRetry(t *testing.T, from *common.KeibiDrop, req *bindings.NotifyRequest, what string) {
	t.Helper()
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		client := from.KDClient
		if client == nil {
			return false
		}
		_, err := client.Notify(context.Background(), req)
		return err == nil
	}, what)
}

// waitRemoteAbsent blocks until name is gone from the peer's RemoteFiles map.
func waitRemoteAbsent(t *testing.T, kd *common.KeibiDrop, name string, timeout time.Duration) {
	t.Helper()
	WaitForCondition(t, timeout, 100*time.Millisecond, func() bool {
		kd.SyncTracker.RemoteFilesMu.RLock()
		defer kd.SyncTracker.RemoteFilesMu.RUnlock()
		_, ok := kd.SyncTracker.RemoteFiles[name]
		return !ok
	}, "waiting for remote file to disappear: "+name)
}

// remoteFilePresent reports whether name is currently in the peer's RemoteFiles map.
func remoteFilePresent(kd *common.KeibiDrop, name string) bool {
	kd.SyncTracker.RemoteFilesMu.RLock()
	defer kd.SyncTracker.RemoteFilesMu.RUnlock()
	_, ok := kd.SyncTracker.RemoteFiles[name]
	return ok
}

// waitRemoteKeyBySuffix blocks until a RemoteFiles key ending in suffix appears and
// returns that exact key. FUSE writes on the mount reach the no-FUSE peer under an
// absolute "/dir/file" key; matching on suffix keeps the delete assertions robust to
// the leading-slash convention while still capturing the real key to assert against.
func waitRemoteKeyBySuffix(t *testing.T, kd *common.KeibiDrop, suffix string, timeout time.Duration) string {
	t.Helper()
	var found string
	WaitForCondition(t, timeout, 100*time.Millisecond, func() bool {
		kd.SyncTracker.RemoteFilesMu.RLock()
		defer kd.SyncTracker.RemoteFilesMu.RUnlock()
		for k := range kd.SyncTracker.RemoteFiles {
			if strings.HasSuffix(k, suffix) {
				found = k
				return true
			}
		}
		return false
	}, "waiting for remote key with suffix: "+suffix)
	return found
}

// TestRekeyRegression_NoFUSE_RenameAcrossRotation shares a file, forces one rotation
// while idle, then renames the file on the source over the re-handshaked session and
// asserts the peer reflects the rename. It also proves the fresh session still carries
// byte-identical data (post-rotation round trip), so the rename is exercised on a
// genuinely rotated session, not the original one.
func TestRekeyRegression_NoFUSE_RenameAcrossRotation(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Share the file that will be renamed. It must exist on both sides and be idle
	// before we force the rotation (a pending notify would defer the rekey).
	content := []byte("a document that gets renamed across a forward-secrecy rotation")
	src := filepath.Join(tp.AliceSaveDir, "renamable.txt")
	require.NoError(os.WriteFile(src, content, 0644))
	require.NoError(tp.Alice.AddFile(src))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "renamable.txt", 10*time.Second)

	// One forced rotation while idle.
	forceOneRotation(t, tp, initiator, responder, watcher, 40*time.Second)

	// The re-handshaked session still carries byte-identical data.
	rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, "flows-after-rekey.txt", []byte("bytes flow on the rotated session"))

	// Rename over the fresh session: the peer must move the entry to the new name and
	// preserve its size, and drop the old name.
	notifyWithRetry(t, tp.Alice, &bindings.NotifyRequest{
		Type:    bindings.NotifyType(types.RenameFile),
		Path:    "renamed.txt",
		OldPath: "renamable.txt",
		Attr:    &bindings.Attr{Size: int64(len(content))},
	}, "rename renamable.txt -> renamed.txt on the rotated session")

	WaitForRemoteFile(t, tp.Bob.SyncTracker, "renamed.txt", 10*time.Second)
	waitRemoteAbsent(t, tp.Bob, "renamable.txt", 10*time.Second)

	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	renamed := tp.Bob.SyncTracker.RemoteFiles["renamed.txt"]
	tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	require.NotNil(renamed, "renamed entry must exist on the peer")
	require.Equal(uint64(len(content)), renamed.Size, "renamed entry must preserve the file size")
}

// TestRekeyRegression_NoFUSE_RemoveAcrossRotation shares two files, forces one
// rotation, then removes one over the fresh session and asserts the peer drops the
// removed one and keeps the other, byte-identical.
func TestRekeyRegression_NoFUSE_RemoveAcrossRotation(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	keepContent := []byte("this file survives the removal of its sibling")
	dropContent := []byte("this file is removed after the rotation")
	keepSrc := filepath.Join(tp.AliceSaveDir, "keep.txt")
	dropSrc := filepath.Join(tp.AliceSaveDir, "drop.txt")
	require.NoError(os.WriteFile(keepSrc, keepContent, 0644))
	require.NoError(os.WriteFile(dropSrc, dropContent, 0644))
	require.NoError(tp.Alice.AddFile(keepSrc))
	require.NoError(tp.Alice.AddFile(dropSrc))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "keep.txt", 10*time.Second)
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "drop.txt", 10*time.Second)

	// One forced rotation while idle.
	forceOneRotation(t, tp, initiator, responder, watcher, 40*time.Second)

	// After the rotation the initiator re-notifies its shared files, so both should
	// still be visible before we remove one.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "keep.txt", 15*time.Second)
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "drop.txt", 15*time.Second)

	// Remove drop.txt over the fresh session.
	notifyWithRetry(t, tp.Alice, &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.RemoveFile),
		Path: "drop.txt",
	}, "remove drop.txt on the rotated session")

	waitRemoteAbsent(t, tp.Bob, "drop.txt", 10*time.Second)
	require.True(remoteFilePresent(tp.Bob, "keep.txt"), "the un-removed file must remain on the peer")

	// The kept file must still transfer byte-identical on the rotated session.
	keepDst := filepath.Join(tp.BobSaveDir, "keep-pulled.txt")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.PullFile("keep.txt", keepDst) == nil
	}, "pull the kept file after removing its sibling")
	got, err := os.ReadFile(keepDst)
	require.NoError(err)
	require.Equal(keepContent, got, "kept file bytes must be identical after the sibling removal")
}

// TestRekeyRegression_FUSE_NestedDirDeleteAcrossRotation builds a nested directory
// tree with files on the FUSE peer's mount, forces one rotation, then deletes a
// subtree on the mount and asserts the no-FUSE peer drops exactly the files under the
// deleted subtree while the sibling file survives byte-identical. This exercises the
// FUSE local-change -> peer notify path on the freshly re-handshaked session. It
// asserts on the files (not the directory entries): a stale-empty-dir edge case is a
// known pre-existing issue and is out of scope for this rotation regression.
func TestRekeyRegression_FUSE_NestedDirDeleteAcrossRotation(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	if testing.Short() {
		t.Skip("FUSE rekey test skipped in short mode")
	}

	t.Setenv("KD_PROACTIVE_REKEY", "1")

	tp := SetupFUSEPeerPair(t, 90*time.Second) // Alice=FUSE, Bob=no-FUSE
	require := require.New(t)
	requireResilienceReady(t, tp)
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Build proj/sub/{a,b}.txt plus a sibling proj/keep.txt on Alice's mount.
	subDir := filepath.Join(tp.AliceMountDir, "proj", "sub")
	require.NoError(os.MkdirAll(subDir, 0755))
	aContent := []byte("nested file a under the deleted subtree")
	bContent := []byte("nested file b under the deleted subtree")
	keepContent := []byte("sibling file that must survive the subtree delete")
	require.NoError(os.WriteFile(filepath.Join(subDir, "a.txt"), aContent, 0644))
	require.NoError(os.WriteFile(filepath.Join(subDir, "b.txt"), bContent, 0644))
	require.NoError(os.WriteFile(filepath.Join(tp.AliceMountDir, "proj", "keep.txt"), keepContent, 0644))

	// Capture the exact keys the no-FUSE peer sees, so the delete assertion checks the
	// real entries rather than a guessed path format.
	keyA := waitRemoteKeyBySuffix(t, tp.Bob, "proj/sub/a.txt", 20*time.Second)
	keyB := waitRemoteKeyBySuffix(t, tp.Bob, "proj/sub/b.txt", 20*time.Second)
	keyKeep := waitRemoteKeyBySuffix(t, tp.Bob, "proj/keep.txt", 20*time.Second)

	// One forced rotation. The initiator retry waits out the ADD_DIR/ADD_FILE notify
	// queue from building the tree (the #6 guard defers a rekey while notifies pend).
	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

	// Delete the subtree on the mount over the fresh session.
	require.NoError(os.RemoveAll(subDir))

	// The peer must drop exactly the two files under the deleted subtree.
	waitRemoteAbsent(t, tp.Bob, keyA, 20*time.Second)
	waitRemoteAbsent(t, tp.Bob, keyB, 20*time.Second)
	require.True(remoteFilePresent(tp.Bob, keyKeep), "the sibling file must survive the subtree delete")

	// The surviving sibling must still transfer byte-identical on the rotated session.
	keepDst := filepath.Join(tp.BobSaveDir, "keep-from-mount.txt")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.PullFile(keyKeep, keepDst) == nil
	}, "pull the surviving sibling after the subtree delete")
	got, err := os.ReadFile(keepDst)
	require.NoError(err)
	require.Equal(keepContent, got, "surviving sibling bytes must be identical after the subtree delete")
}

// rekeyRegressionBigFileSize is a large (>=64MB) transfer used to prove integrity on a
// freshly re-handshaked session. It stays well under the 1GB rekey byte threshold so
// the background health tick never auto-rotates and races the test.
const rekeyRegressionBigFileSize = 64 * 1024 * 1024

// TestRekeyRegression_NoFUSE_LargeFileAfterRotation rotates FIRST while idle, THEN
// pulls a large (>=64MB) file over the re-handshaked session and asserts the bytes are
// identical. This is distinct from the during-transfer F3 defer (already covered): it
// proves a big transfer stays intact when it runs entirely on a brand-new session.
func TestRekeyRegression_NoFUSE_LargeFileAfterRotation(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")

	// A large transfer plus a reconnect needs headroom so the peer context never tears
	// the peers down mid-test. Only widens the time budget, not any assertion.
	tp := SetupPeerPairWithTimeout(t, false, 90*time.Second)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Rotate first, while the link is idle.
	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

	// Now push a large file over the freshly re-handshaked session and pull it back.
	big := make([]byte, rekeyRegressionBigFileSize)
	for i := range big {
		big[i] = byte(i%251 + 1) // never zero, so a truncated (zero) file never matches
	}
	const bigName = "big-after-rekey.bin"
	src := filepath.Join(tp.AliceSaveDir, bigName)
	require.NoError(os.WriteFile(src, big, 0644))
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return tp.Alice.AddFile(src) == nil
	}, "share the large file on the rotated session")
	WaitForRemoteFile(t, tp.Bob.SyncTracker, bigName, 15*time.Second)

	dst := filepath.Join(tp.BobSaveDir, bigName)
	WaitForCondition(t, 60*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.PullFile(bigName, dst) == nil
	}, "pull the large file on the rotated session")

	got, err := os.ReadFile(dst)
	require.NoError(err)
	require.Equal(len(big), len(got), "large file size mismatch after rotation")
	require.True(bytes.Equal(big, got), "large file bytes must be identical after rotation")
}

// TestRekeyRegression_NoFUSE_MultiFileConcurrentAcrossRotation rotates first, then
// shares several files at once over the fresh session and asserts every one arrives
// byte-identical. It guards against a rotation corrupting concurrent shares.
func TestRekeyRegression_NoFUSE_MultiFileConcurrentAcrossRotation(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")

	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Rotate first, while idle.
	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

	// Build several distinct files on disk before sharing them.
	const nFiles = 6
	names := make([]string, nFiles)
	contents := make([][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		names[i] = fmt.Sprintf("multi_%d.bin", i)
		b := make([]byte, 128*1024)
		for j := range b {
			b[j] = byte((i*7+j)%251 + 1)
		}
		contents[i] = b
		require.NoError(os.WriteFile(filepath.Join(tp.AliceSaveDir, names[i]), b, 0644))
	}

	// Share them all at once over the rotated session. Each goroutine retries its own
	// AddFile (the server may still be restarting) and records only its final error;
	// t.Fatal must stay on the test goroutine, so we assert after the WaitGroup.
	errs := make([]error, nFiles)
	var wg sync.WaitGroup
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			src := filepath.Join(tp.AliceSaveDir, names[idx])
			deadline := time.Now().Add(20 * time.Second)
			var err error
			for time.Now().Before(deadline) {
				if err = tp.Alice.AddFile(src); err == nil {
					return
				}
				time.Sleep(150 * time.Millisecond)
			}
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i := 0; i < nFiles; i++ {
		require.NoError(errs[i], "share %s on the rotated session", names[i])
	}

	// Every file must arrive byte-identical on the peer.
	for i := 0; i < nFiles; i++ {
		WaitForRemoteFile(t, tp.Bob.SyncTracker, names[i], 15*time.Second)
		dst := filepath.Join(tp.BobSaveDir, names[i])
		WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
			return tp.Bob.PullFile(names[i], dst) == nil
		}, "pull "+names[i]+" on the rotated session")
		got, err := os.ReadFile(dst)
		require.NoError(err)
		require.True(bytes.Equal(contents[i], got), "bytes for %s must be identical after rotation", names[i])
	}
}

// TestRekeyRegression_FUSE_GitCloneAcrossRotation is the headline real-app stretch
// case. It is intentionally skipped: forcing a proactive rekey needs the in-process
// HealthMonitor.OnRekeyNeeded probe, but a realistic FUSE-to-FUSE git clone runs both
// peers as separate testpeer subprocesses (see integration_fuse_git_test.go), which
// expose no command to inject a rotation. Driving a real clone into the single
// in-process FUSE mount while forcing a reconnect mid-clone is timing-heavy and flaky,
// so per the task guidance we skip rather than ship a flaky test. The rotated-session
// FUSE data and delete paths are covered by
// TestRekeyRegression_FUSE_NestedDirDeleteAcrossRotation and, for a fresh receive,
// TestRekeyRotation_FUSE_RotatesAndDataFlows.
func TestRekeyRegression_FUSE_GitCloneAcrossRotation(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	t.Skip("stretch case skipped: the FUSE-to-FUSE git-clone harness spawns peers as subprocesses " +
		"with no rekey-injection command, and an in-process clone-with-mid-clone-reconnect is too flaky to ship")
}
