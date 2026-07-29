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

// forceOneRotation drives exactly one proactive rekey on the initiator and blocks until
// both peers have re-handshaked. It probes until the initiator rotates: OnRekeyNeeded
// returns false while a notify is still queued (the #6 guard), and a false return has no
// side effects, so repeated probing is safe.
func forceOneRotation(t *testing.T, tp *TestPair, initiator, responder *common.KeibiDrop, w *reconnectWatcher, timeout time.Duration) {
	t.Helper()
	require.False(t, responder.HealthMonitor.OnRekeyNeeded(), "responder (non-initiator) must not rotate")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return initiator.HealthMonitor.OnRekeyNeeded()
	}, "initiator to initiate the one forced rotation")
	w.awaitBoth(t, tp, timeout)
}

// notifyWithRetry sends a notification, retrying until it succeeds: right after a rotation
// the peer's gRPC client is still being rebuilt (or briefly nil). The far handler is
// idempotent for these ops, so a retried delivery is harmless.
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

// waitRemoteKeyBySuffix blocks until a RemoteFiles key ending in suffix appears and returns
// it. FUSE writes reach the no-FUSE peer under an absolute "/dir/file" key, so matching on
// suffix stays robust to the leading-slash convention while capturing the real key.
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

// TestRekeyRegression_NoFUSE_RenameAcrossRotation shares a file, forces one rotation while
// idle, then renames it over the re-handshaked session and asserts the peer reflects the rename.
func TestRekeyRegression_NoFUSE_RenameAcrossRotation(t *testing.T) {
	disableKeyUpdate(t)

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Share the file to rename; it must be idle before the rotation (a pending notify defers the rekey).
	content := []byte("a document that gets renamed across a forward-secrecy rotation")
	src := filepath.Join(tp.AliceSaveDir, "renamable.txt")
	require.NoError(os.WriteFile(src, content, 0644))
	require.NoError(tp.Alice.AddFile(src))
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "renamable.txt", 10*time.Second)

	forceOneRotation(t, tp, initiator, responder, watcher, 40*time.Second)

	rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, "flows-after-rekey.txt", []byte("bytes flow on the rotated session"))

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

// TestRekeyRegression_NoFUSE_RemoveAcrossRotation shares two files, forces one rotation, then
// removes one over the fresh session and asserts the peer drops it and keeps the other.
func TestRekeyRegression_NoFUSE_RemoveAcrossRotation(t *testing.T) {
	disableKeyUpdate(t)

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

	forceOneRotation(t, tp, initiator, responder, watcher, 40*time.Second)

	// After the rotation the initiator re-notifies its shared files, so both are visible again.
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "keep.txt", 15*time.Second)
	WaitForRemoteFile(t, tp.Bob.SyncTracker, "drop.txt", 15*time.Second)

	notifyWithRetry(t, tp.Alice, &bindings.NotifyRequest{
		Type: bindings.NotifyType(types.RemoveFile),
		Path: "drop.txt",
	}, "remove drop.txt on the rotated session")

	waitRemoteAbsent(t, tp.Bob, "drop.txt", 10*time.Second)
	require.True(remoteFilePresent(tp.Bob, "keep.txt"), "the un-removed file must remain on the peer")

	keepDst := filepath.Join(tp.BobSaveDir, "keep-pulled.txt")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.PullFile("keep.txt", keepDst) == nil
	}, "pull the kept file after removing its sibling")
	got, err := os.ReadFile(keepDst)
	require.NoError(err)
	require.Equal(keepContent, got, "kept file bytes must be identical after the sibling removal")
}

// TestRekeyRegression_FUSE_NestedDirDeleteAcrossRotation builds a nested tree on the FUSE
// mount, forces one rotation, then deletes a subtree and asserts the no-FUSE peer drops
// exactly those files while the sibling survives. It asserts on files, not directory
// entries: a stale-empty-dir edge case is a known pre-existing issue, out of scope here.
func TestRekeyRegression_FUSE_NestedDirDeleteAcrossRotation(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	if testing.Short() {
		t.Skip("FUSE rekey test skipped in short mode")
	}

	disableKeyUpdate(t)

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

	// Capture the exact keys the no-FUSE peer sees, so the delete assertion checks real entries.
	keyA := waitRemoteKeyBySuffix(t, tp.Bob, "proj/sub/a.txt", 20*time.Second)
	keyB := waitRemoteKeyBySuffix(t, tp.Bob, "proj/sub/b.txt", 20*time.Second)
	keyKeep := waitRemoteKeyBySuffix(t, tp.Bob, "proj/keep.txt", 20*time.Second)

	// One forced rotation. The initiator retry waits out the ADD_DIR/ADD_FILE notify queue
	// (the #6 guard defers a rekey while notifies pend).
	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

	require.NoError(os.RemoveAll(subDir))

	waitRemoteAbsent(t, tp.Bob, keyA, 20*time.Second)
	waitRemoteAbsent(t, tp.Bob, keyB, 20*time.Second)
	require.True(remoteFilePresent(tp.Bob, keyKeep), "the sibling file must survive the subtree delete")

	keepDst := filepath.Join(tp.BobSaveDir, "keep-from-mount.txt")
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return tp.Bob.PullFile(keyKeep, keepDst) == nil
	}, "pull the surviving sibling after the subtree delete")
	got, err := os.ReadFile(keepDst)
	require.NoError(err)
	require.Equal(keepContent, got, "surviving sibling bytes must be identical after the subtree delete")
}

// rekeyRegressionBigFileSize is a large (>=64MB) transfer, kept well under the 1GB rekey
// byte threshold so the background health tick never auto-rotates and races the test.
const rekeyRegressionBigFileSize = 64 * 1024 * 1024

// TestRekeyRegression_NoFUSE_LargeFileAfterRotation rotates first while idle, then pulls a
// large (>=64MB) file over the re-handshaked session and asserts the bytes are identical.
func TestRekeyRegression_NoFUSE_LargeFileAfterRotation(t *testing.T) {
	disableKeyUpdate(t)

	// A large transfer plus a reconnect needs headroom so the peer context never tears the
	// peers down mid-test; this only widens the time budget, not any assertion.
	tp := SetupPeerPairWithTimeout(t, false, 90*time.Second)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

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

// TestRekeyRegression_NoFUSE_MultiFileConcurrentAcrossRotation rotates first, then shares
// several files at once over the fresh session and asserts every one arrives byte-identical.
func TestRekeyRegression_NoFUSE_MultiFileConcurrentAcrossRotation(t *testing.T) {
	disableKeyUpdate(t)

	tp := SetupPeerPairWithTimeout(t, false, 60*time.Second)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	forceOneRotation(t, tp, initiator, responder, watcher, 45*time.Second)

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

	// Share them all at once. Each goroutine retries its own AddFile (the server may still be
	// restarting) and records only its final error; t.Fatal must stay on the test goroutine, so
	// we assert after the WaitGroup.
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

// TestRekeyRegression_FUSE_GitCloneAcrossRotation is intentionally skipped: forcing a
// proactive rekey needs the in-process OnRekeyNeeded probe, but a realistic FUSE-to-FUSE
// git clone runs both peers as subprocesses with no command to inject a rotation, and an
// in-process clone-with-mid-clone-reconnect is too flaky to ship.
func TestRekeyRegression_FUSE_GitCloneAcrossRotation(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	t.Skip("stretch case skipped: the FUSE-to-FUSE git-clone harness spawns peers as subprocesses " +
		"with no rekey-injection command, and an in-process clone-with-mid-clone-reconnect is too flaky to ship")
}
