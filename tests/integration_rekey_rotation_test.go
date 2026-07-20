// ABOUTME: End-to-end proactive forward-secrecy rekey tests: one forced re-handshake
// ABOUTME: rotates the session keys and data still flows (non-FUSE, FUSE, mid-transfer defer).

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// disableKeyUpdate turns the in-band ratchet off for a test, so a same-build pair negotiates
// key-update off and exercises the re-handshake fallback (the path the rotation and
// regression tests assert). Restored on cleanup.
func disableKeyUpdate(t *testing.T) {
	t.Helper()
	session.SetKeyUpdateEnabled(false)
	t.Cleanup(func() { session.SetKeyUpdateEnabled(true) })
}

// rekeyRoles returns the pair's deterministic reconnect roles. Exactly one peer
// (the lexicographically lower fingerprint) is the initiator that drives the
// re-handshake; the other follows via its normal disconnect recovery.
func rekeyRoles(t *testing.T, tp *TestPair) (initiator, responder *common.KeibiDrop) {
	t.Helper()
	aliceInit := tp.Alice.ReconnectManager.IsReconnectInitiator()
	bobInit := tp.Bob.ReconnectManager.IsReconnectInitiator()
	require.NotEqual(t, aliceInit, bobInit, "exactly one peer must be the reconnect initiator")
	if aliceInit {
		return tp.Alice, tp.Bob
	}
	return tp.Bob, tp.Alice
}

// saveDirFor returns the save directory of the given peer within the pair.
func saveDirFor(tp *TestPair, peer *common.KeibiDrop) string {
	if peer == tp.Alice {
		return tp.AliceSaveDir
	}
	return tp.BobSaveDir
}

// requireResilienceReady asserts both peers finished wiring their health monitor
// and reconnect manager during setup, so the rekey/reconnect API is safe to use.
func requireResilienceReady(t *testing.T, tp *TestPair) {
	t.Helper()
	require.NotNil(t, tp.Alice.HealthMonitor, "alice health monitor")
	require.NotNil(t, tp.Bob.HealthMonitor, "bob health monitor")
	require.NotNil(t, tp.Alice.ReconnectManager, "alice reconnect manager")
	require.NotNil(t, tp.Bob.ReconnectManager, "bob reconnect manager")
}

// reconnectWatcher captures each peer's "reconnected:" event.
//
// The automatic reconnect loop stores the "connected" reconnect state BEFORE
// onReconnected rebuilds the gRPC client (reconnect.go sets the state, then calls
// OnReconnected). So polling ReconnectionState()=="connected" observes the link
// before the fresh client exists and races the rebuild (a real DATA RACE on
// session.GRPCClient). The "reconnected:" event is emitted AFTER the rebuild, and
// delivering it over a channel gives the test a clean happens-before to the live
// session. Both peers rebuild independently, so both events must be awaited.
type reconnectWatcher struct {
	alice chan struct{}
	bob   chan struct{}
}

// watchReconnect wires the per-peer OnEvent callbacks. Call it before triggering a
// rotation (and before the baseline round trip, so the OnEvent field write is
// ordered ahead of the monitor goroutines that later read it).
func watchReconnect(tp *TestPair) *reconnectWatcher {
	w := &reconnectWatcher{
		alice: make(chan struct{}, 8),
		bob:   make(chan struct{}, 8),
	}
	tp.Alice.OnEvent = func(ev string) {
		if strings.HasPrefix(ev, "reconnected:") {
			select {
			case w.alice <- struct{}{}:
			default:
			}
		}
	}
	tp.Bob.OnEvent = func(ev string) {
		if strings.HasPrefix(ev, "reconnected:") {
			select {
			case w.bob <- struct{}{}:
			default:
			}
		}
	}
	return w
}

// awaitBoth blocks until both peers have finished rebuilding their session after
// the forced re-handshake, then asserts both report the connected state.
func (w *reconnectWatcher) awaitBoth(t *testing.T, tp *TestPair, timeout time.Duration) {
	t.Helper()
	waitEvent(t, w.alice, timeout, "alice reconnected event")
	waitEvent(t, w.bob, timeout, "bob reconnected event")
	require.Equal(t, "connected", tp.Alice.ReconnectionState(), "alice reconnect state")
	require.Equal(t, "connected", tp.Bob.ReconnectionState(), "bob reconnect state")
}

func waitEvent(t *testing.T, ch <-chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// rekeyRoundTrip shares content from "from" and pulls it on "to", asserting the
// bytes survived the trip. It exercises one nonce direction on whatever session
// keys are currently live, so calling it before and after a rotation proves the
// re-handshaked session actually carries data. The share step is retried because
// right after a reconnect the peer's gRPC server is (re)starting asynchronously.
func rekeyRoundTrip(t *testing.T, from *common.KeibiDrop, fromSave string, to *common.KeibiDrop, toSave, name string, content []byte) {
	t.Helper()
	require := require.New(t)
	src := filepath.Join(fromSave, name)
	require.NoError(os.WriteFile(src, content, 0644))
	// AddFile is an upsert, so retrying until the peer's server is serving again
	// is safe and needs no sleep.
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return from.AddFile(src) == nil
	}, "share "+name+" with peer")
	WaitForRemoteFile(t, to.SyncTracker, name, 15*time.Second)
	dst := filepath.Join(toSave, name)
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return to.PullFile(name, dst) == nil
	}, "pull "+name+" from peer")
	got, err := os.ReadFile(dst)
	require.NoError(err)
	require.Equal(content, got)
}

// rekeyRoundTripToMount shares content from the no-FUSE peer and verifies it
// materializes byte-identical on the FUSE peer's mount. The share is retried
// (post-reconnect server restart) and the mount read is polled (on-demand
// streaming may need a beat to serve the full file) rather than slept.
func rekeyRoundTripToMount(t *testing.T, from *common.KeibiDrop, fromSave, mountDir, name string, content []byte) {
	t.Helper()
	require := require.New(t)
	src := filepath.Join(fromSave, name)
	require.NoError(os.WriteFile(src, content, 0644))
	WaitForCondition(t, 20*time.Second, 200*time.Millisecond, func() bool {
		return from.AddFile(src) == nil
	}, "share "+name+" with FUSE peer")
	mp := filepath.Join(mountDir, name)
	WaitForFileOnMount(t, mp, 15*time.Second)
	var got []byte
	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		b, err := os.ReadFile(mp)
		if err != nil {
			return false
		}
		got = b
		return bytes.Equal(b, content)
	}, "byte-identical read on FUSE mount: "+name)
	require.Equal(content, got)
}

// TestRekeyRotation_NoFUSE_RotatesAndDataFlows forces exactly one proactive
// forward-secrecy rotation on the initiator and proves the re-handshaked session
// still carries data in both directions. The guard (only the initiator rotates)
// and the load-bearing post-rotation round trips are both asserted.
func TestRekeyRotation_NoFUSE_RotatesAndDataFlows(t *testing.T) {
	// Exercise the re-handshake fallback: disable the ratchet so the pair rotates via a
	// re-handshake (read at setup time, before the pair connects).
	disableKeyUpdate(t)

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)

	// One failed heartbeat (~1 Interval) is enough for the responder to notice the
	// initiator dropped the sockets, so the automatic reconnect starts promptly
	// instead of after the default five failures.
	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Baseline: the pre-rotation session carries data both ways.
	rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, "pre-a2b.txt", []byte("alice to bob before rekey"))
	rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir, "pre-b2a.txt", []byte("bob to alice before rekey"))

	// Only the initiator rotates. The responder's call is a guarded no-op.
	require.False(responder.HealthMonitor.OnRekeyNeeded(), "responder (non-initiator) must not rotate")
	require.True(initiator.HealthMonitor.OnRekeyNeeded(), "initiator must initiate the rotation")

	// The forced re-handshake must bring both peers back with a fresh session.
	watcher.awaitBoth(t, tp, 30*time.Second)

	// Load-bearing: the rotated (re-handshaked) session carries data both ways.
	rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, "post-a2b.txt", []byte("alice to bob AFTER rekey"))
	rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir, "post-b2a.txt", []byte("bob to alice AFTER rekey"))
}

// TestRekeyRotation_NoFUSE_RatchetRotatesInBand proves the new-new path: with the in-band
// ratchet negotiated (the default), the session rotates its keys mid-connection without
// dropping the sockets. It lowers the ratchet threshold, moves several MiB across it in both
// directions, and asserts the key epoch advanced with zero reconnect events, so the rotation
// was in-band, not a re-handshake.
func TestRekeyRotation_NoFUSE_RatchetRotatesInBand(t *testing.T) {
	// Ratchet ON (the default: do NOT disableKeyUpdate). Force ratchets on small transfers by
	// dropping the byte threshold to 1 MiB; restore it so no later test inherits it.
	origBytes, origMsgs := session.RekeyBytesThreshold, session.RekeyMsgsThreshold
	session.RekeyBytesThreshold = 1 << 20
	t.Cleanup(func() { session.RekeyBytesThreshold, session.RekeyMsgsThreshold = origBytes, origMsgs })

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)

	initiator, responder := rekeyRoles(t, tp)

	// The pair negotiated the ratchet, so the re-handshake fallback is demoted: OnRekeyNeeded
	// must be a no-op on BOTH peers (it would needlessly drop the sockets).
	require.False(initiator.HealthMonitor.OnRekeyNeeded(), "ratchet on: initiator must not re-handshake")
	require.False(responder.HealthMonitor.OnRekeyNeeded(), "ratchet on: responder must not re-handshake")

	watcher := watchReconnect(tp)
	epochBefore := initiator.HealthMonitor.MaxWriterEpoch()

	// Move several MiB both ways across the 1 MiB threshold. Each transfer survives only if the
	// writer's mid-stream epoch bump and the reader's follow both work with no socket drop.
	payload := bytes.Repeat([]byte("ratchet-payload!"), 96*1024) // ~1.5 MiB
	for i := 0; i < 4; i++ {
		rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir,
			fmt.Sprintf("ratchet-a2b-%d.bin", i), payload)
		rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir,
			fmt.Sprintf("ratchet-b2a-%d.bin", i), payload)
	}

	// The key epoch advanced mid-connection: the in-band ratchet rotated the keys.
	require.Greater(initiator.HealthMonitor.MaxWriterEpoch(), epochBefore, "ratchet must advance the epoch")

	// And with no socket drop: neither peer reconnected.
	select {
	case <-watcher.alice:
		t.Fatal("alice reconnected: the ratchet rotation must not drop the sockets")
	case <-watcher.bob:
		t.Fatal("bob reconnected: the ratchet rotation must not drop the sockets")
	default:
	}
	require.Equal("connected", tp.Alice.ReconnectionState())
	require.Equal("connected", tp.Bob.ReconnectionState())
}

// TestRekeyRotation_FUSE_RotatesAndDataFlows is the FUSE-pair analogue: force one
// rotation and prove data still flows onto the FUSE peer's mount afterwards.
func TestRekeyRotation_FUSE_RotatesAndDataFlows(t *testing.T) {
	if !isFUSEPresent() {
		t.Skip("FUSE not available on this platform")
	}
	if testing.Short() {
		t.Skip("FUSE rekey test skipped in short mode")
	}

	disableKeyUpdate(t)

	tp := SetupFUSEPeerPair(t, 60*time.Second) // Alice=FUSE, Bob=no-FUSE
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)

	// Baseline FUSE receive: Bob (no-FUSE) shares, Alice sees it on her mount.
	rekeyRoundTripToMount(t, tp.Bob, tp.BobSaveDir, tp.AliceMountDir, "fuse-pre.txt", []byte("bob to alice fuse before rekey"))

	require.False(responder.HealthMonitor.OnRekeyNeeded(), "responder (non-initiator) must not rotate")
	require.True(initiator.HealthMonitor.OnRekeyNeeded(), "initiator must initiate the rotation")

	watcher.awaitBoth(t, tp, 40*time.Second)

	// Load-bearing: a file shared AFTER the rotation must materialize on the FUSE
	// peer's mount, not just in SyncTracker. This is the regression guard for the
	// reconnect-rebuild dropping the service's FUSE wiring.
	rekeyRoundTripToMount(t, tp.Bob, tp.BobSaveDir, tp.AliceMountDir, "fuse-post.txt", []byte("bob to alice fuse AFTER rekey"))
}

// rekeyBigFileSize is large enough that the pull is still streaming when we probe
// the rekey guard, yet well under the 1GB rekey byte threshold so the background
// health tick never auto-rotates and races the test.
const rekeyBigFileSize = 128 * 1024 * 1024

// TestRekeyRotation_NoFUSE_DefersDuringActiveTransfer is the end-to-end F3 guard:
// a proactive rekey must never drop the sockets while a transfer is in flight. We
// drive a real large pull on the initiator, and while it streams every rekey probe
// must defer; the transfer must complete intact; and only once idle may a rotation
// succeed and data flow again.
func TestRekeyRotation_NoFUSE_DefersDuringActiveTransfer(t *testing.T) {
	disableKeyUpdate(t)

	// A large transfer plus a reconnect needs more wall-clock than the 30s default
	// peer context; give it headroom so the context never tears the peers down
	// mid-test. This only widens the time budget, not any assertion.
	tp := SetupPeerPairWithTimeout(t, false, 90*time.Second)
	require := require.New(t)
	requireResilienceReady(t, tp)

	tp.Alice.HealthMonitor.MaxFailures = 1
	tp.Bob.HealthMonitor.MaxFailures = 1

	watcher := watchReconnect(tp)
	initiator, responder := rekeyRoles(t, tp)
	initSave := saveDirFor(tp, initiator)
	respSave := saveDirFor(tp, responder)

	// A rotation is driven by the initiator's own hasActiveTransfers() gate, which
	// tracks downloads (pulls). So the initiator must be the puller: the responder
	// serves a large file and the initiator pulls it.
	big := make([]byte, rekeyBigFileSize)
	for i := range big {
		big[i] = byte(i%251 + 1) // never zero, so a truncated (zero) file never matches
	}
	const bigName = "big-transfer.bin"
	require.NoError(os.WriteFile(filepath.Join(respSave, bigName), big, 0644))
	require.NoError(responder.AddFile(filepath.Join(respSave, bigName)))
	WaitForRemoteFile(t, initiator.SyncTracker, bigName, 15*time.Second)

	bigDest := filepath.Join(initSave, bigName)
	pullDone := make(chan error, 1)
	go func() { pullDone <- initiator.PullFile(bigName, bigDest) }()

	// Wait until the pull has genuinely started streaming before probing the guard.
	// PullFile registers the download before streaming, and pullStreamFile writes
	// chunks sequentially from offset 0, so a matching head means the download is
	// registered (active) and probing exercises the F3 gate rather than firing
	// before the transfer exists.
	head := big[:1024]
	WaitForCondition(t, 20*time.Second, 20*time.Millisecond, func() bool {
		select {
		case err := <-pullDone:
			// Finished before we could observe it streaming (file too small). Push
			// it back so the loop below reports it and the size assertion fails loudly.
			pullDone <- err
			return true
		default:
		}
		f, err := os.Open(bigDest)
		if err != nil {
			return false
		}
		defer f.Close()
		buf := make([]byte, len(head))
		if _, err := io.ReadFull(f, buf); err != nil {
			return false
		}
		return bytes.Equal(buf, head)
	}, "large download to start streaming")

	// While the transfer is in flight, every rekey probe must defer (F3). A probe
	// may only succeed once the transfer has finished, so a true result is accepted
	// solely if the pull is draining right now; otherwise the guard let a rotation
	// cut a live transfer, which is the exact regression this test guards.
	deferrals := 0
	rotatedAtTail := false
	var transferErr error
poll:
	for {
		select {
		case transferErr = <-pullDone:
			break poll
		default:
		}
		if initiator.HealthMonitor.OnRekeyNeeded() {
			select {
			case transferErr = <-pullDone:
				rotatedAtTail = true
			case <-time.After(10 * time.Second):
				t.Fatal("F3 violation: proactive rekey rotated while a transfer was still in flight")
			}
			break poll
		}
		deferrals++
	}
	require.NoError(transferErr, "the large pull must complete intact (a rekey must not cut it)")
	require.Greater(deferrals, 0, "no active-transfer deferral observed; increase rekeyBigFileSize")

	got, err := os.ReadFile(bigDest)
	require.NoError(err)
	require.True(bytes.Equal(big, got), "pulled bytes must be identical to the source")

	// The link is idle now, so a rotation must succeed. If it already fired at the
	// tail of the transfer, the 60s cooldown blocks a second one, so skip it.
	if !rotatedAtTail {
		require.True(initiator.HealthMonitor.OnRekeyNeeded(), "rekey must rotate once the transfer is done")
	}

	watcher.awaitBoth(t, tp, 45*time.Second)

	// Data must flow again on the rotated session, both directions.
	rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, "post-defer-a2b.txt", []byte("alice to bob after deferred rekey"))
	rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir, "post-defer-b2a.txt", []byte("bob to alice after deferred rekey"))
}
