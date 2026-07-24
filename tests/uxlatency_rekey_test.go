// ABOUTME: UX-latency regression guards: forcing the in-band ratchet (and the reconnect
// ABOUTME: fold path) must not add user-perceptible lag to reconnect, first-byte, burst, or edit.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests prove the zero-downtime rekey adds no user-perceptible lag. Each measures a
// user-visible latency with the ratchet forced to rotate constantly versus an unforced
// baseline, confirms the ratchet actually rotated (an epoch delta, else the test is vacuous),
// and uses generous margins so loopback jitter never flakes. The rekey lives at the session
// layer below both FUSE and non-FUSE, so both modes ratchet identically.
const (
	// uxlatSmallFileSize: a small file whose "first block" is the whole file (FUSE fetches it
	// in one shot).
	uxlatSmallFileSize = 64 << 10

	// uxlatForcedRekeyBytes forces the writer to ratchet about every half small-file, so a
	// handful of transfers crosses many key-epochs (bypassing the 1 MiB production floor).
	uxlatForcedRekeyBytes = 32 << 10

	// uxlatMinBumps is the floor of forced epoch advances a test must observe to prove the
	// ratchet actually rotated.
	uxlatMinBumps = 10
)

// uxlatPattern builds size bytes of deterministic seed-dependent content (never zero, so a
// sparse-hole read is distinguishable).
func uxlatPattern(seed, size int) []byte {
	b := make([]byte, size)
	for j := range b {
		b[j] = byte((seed*7+j)%251 + 1)
	}
	return b
}

// uxlatRevision builds a same-size file body whose first 8 bytes encode rev, so a peer can
// tell which in-place revision it fetched (the hard same-size in-place-edit case).
func uxlatRevision(rev, size int) []byte {
	b := make([]byte, size)
	binary.BigEndian.PutUint64(b, uint64(rev)) //nolint:gosec // G115: rev is a small non-negative counter
	for j := 8; j < len(b); j++ {
		b[j] = byte((rev*7+j)%251 + 1)
	}
	return b
}

// uxlatReadWhole opens path and reads exactly size bytes; a short read surfaces as an error,
// catching a truncated on-demand fetch.
func uxlatReadWhole(path string, size int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only handle
	buf := make([]byte, size)
	n, err := io.ReadFull(f, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// uxlatSyncSmallFiles shares nFiles small files Bob->Alice, then on Alice fetches and verifies
// each (FUSE reads off the mount, non-FUSE PullFiles it). Returns per-file fetch latency, total
// transfer time, and the writer-epoch bumps. A fresh pair lets the caller force the ratchet
// threshold before connect (race-free).
func uxlatSyncSmallFiles(t *testing.T, fuse bool, nFiles int, forceRekey bool) (lat []time.Duration, total time.Duration, bumps int) {
	t.Helper()
	if forceRekey {
		forceFrequentRatchet(t, uxlatForcedRekeyBytes)
	}

	var tp *TestPair
	if fuse {
		// Pure on-demand: the read, not the open, triggers (and ratchets) the fetch.
		t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")
		tp = SetupFUSEPeerPair(t, 120*time.Second)
		waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	} else {
		tp = SetupPeerPair(t, false)
	}
	requireResilienceReady(t, tp)
	require := require.New(t)
	getEpoch := maxWriterEpoch(tp)

	// Bob (the no-FUSE sharer in both modes) shares nFiles distinct small files.
	names := make([]string, nFiles)
	contents := make([][]byte, nFiles)
	for i := 0; i < nFiles; i++ {
		names[i] = fmt.Sprintf("uxfile_%05d.bin", i)
		contents[i] = uxlatPattern(i, uxlatSmallFileSize)
		require.NoError(os.WriteFile(filepath.Join(tp.BobSaveDir, names[i]), contents[i], 0644))
		require.NoError(tp.Bob.AddFile(filepath.Join(tp.BobSaveDir, names[i])))
	}
	// Wait for all files to be visible before timing, so the timed phase is data transfer only.
	for i := 0; i < nFiles; i++ {
		if fuse {
			WaitForFileOnMount(t, filepath.Join(tp.AliceMountDir, names[i]), 30*time.Second)
		} else {
			WaitForRemoteFile(t, tp.Alice.SyncTracker, names[i], 30*time.Second)
		}
	}

	lat = make([]time.Duration, nFiles)
	e0 := getEpoch()
	startAll := time.Now()
	for i := 0; i < nFiles; i++ {
		start := time.Now()
		var got []byte
		var err error
		if fuse {
			got, err = uxlatReadWhole(filepath.Join(tp.AliceMountDir, names[i]), uxlatSmallFileSize)
		} else {
			dst := filepath.Join(tp.AliceSaveDir, names[i])
			if err = tp.Alice.PullFile(names[i], dst); err == nil {
				got, err = os.ReadFile(dst)
			}
		}
		lat[i] = time.Since(start)
		require.NoErrorf(err, "fetch %s", names[i])
		require.Truef(bytes.Equal(contents[i], got), "content mismatch on %s", names[i])
	}
	total = time.Since(startAll)
	bumps = int(getEpoch()) - int(e0)
	return lat, total, bumps
}

// uxlatEditPropagation repeatedly rewrites ONE file in place on the sharer (each revision the
// same byte length, the hard same-size case) and measures how long the peer takes to see the
// new content. It uses the non-FUSE re-share path (macFUSE same-size page-cache visibility is
// flaky on loopback), which exercises the identical session-layer ratchet and is deterministic.
// A fresh dst per pull attempt is mandatory: PullFile resume-skips a same-size file already on
// disk, which would otherwise serve the stale revision forever.
func uxlatEditPropagation(t *testing.T, nEdits int, forceRekey bool) (lat []time.Duration, bumps int) {
	t.Helper()
	if forceRekey {
		forceFrequentRatchet(t, uxlatForcedRekeyBytes)
	}
	tp := SetupPeerPair(t, false)
	requireResilienceReady(t, tp)
	require := require.New(t)
	getEpoch := maxWriterEpoch(tp)

	const name = "uxlat_edited.bin"
	src := filepath.Join(tp.BobSaveDir, name)
	// Revision 0 establishes the file on both peers.
	require.NoError(os.WriteFile(src, uxlatRevision(0, uxlatSmallFileSize), 0644))
	require.NoError(tp.Bob.AddFile(src))
	WaitForRemoteFile(t, tp.Alice.SyncTracker, name, 30*time.Second)

	lat = make([]time.Duration, nEdits)
	e0 := getEpoch()
	for i := 1; i <= nEdits; i++ {
		// Same-size in-place edit on the sharer, then re-notify (AddFile upserts).
		require.NoError(os.WriteFile(src, uxlatRevision(i, uxlatSmallFileSize), 0644))
		require.NoError(tp.Bob.AddFile(src))

		editStart := time.Now()
		seen := false
		for attempt := 1; time.Since(editStart) < 30*time.Second; attempt++ {
			dst := filepath.Join(tp.AliceSaveDir, fmt.Sprintf("pulled_%04d_%04d.bin", i, attempt))
			if tp.Alice.PullFile(name, dst) == nil {
				if got, rerr := os.ReadFile(dst); rerr == nil && len(got) >= 8 &&
					binary.BigEndian.Uint64(got[:8]) == uint64(i) { //nolint:gosec // G115: i is a small counter
					seen = true
					break
				}
			}
			time.Sleep(3 * time.Millisecond)
		}
		lat[i-1] = time.Since(editStart)
		require.Truef(seen, "peer never saw edit revision %d within timeout", i)
	}
	bumps = int(getEpoch()) - int(e0)
	return lat, bumps
}

// uxlatForcedReconnect forces one re-handshake reconnect and blocks until both peers rebuild:
// only the initiator drives it, the responder's probe is a guarded no-op, both "reconnected:"
// events must fire. Callers must disableKeyUpdate before setup and wire watcher before the
// baseline transfer. Returns the reconnected instant so the caller times recovery.
func uxlatForcedReconnect(t *testing.T, tp *TestPair, watcher *reconnectWatcher) time.Time {
	t.Helper()
	initiator, responder := rekeyRoles(t, tp)
	require.False(t, responder.HealthMonitor.OnRekeyNeeded(), "responder (non-initiator) must not drive the reconnect")
	require.True(t, initiator.HealthMonitor.OnRekeyNeeded(), "initiator must drive the reconnect")
	watcher.awaitBoth(t, tp, 30*time.Second)
	return time.Now()
}

// TestUXLatency_ReconnectRecovery forces a reconnect and measures recovery from the reconnected
// event to the first successful post-reconnect round trip, asserting it is fast and intact.
//
// Forcing a reconnect uses OnRekeyNeeded, which only drops the sockets when the ratchet is
// disabled (the re-handshake fallback), so this exercises the re-handshake rotation and resume.
// The rotation is proven fired by both "reconnected:" events, not an epoch delta (the ratchet
// is off on this path, so the writer epoch stays at 0).
func TestUXLatency_ReconnectRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping reconnect-recovery latency in short mode")
	}

	t.Run("NoFUSE", func(t *testing.T) {
		disableKeyUpdate(t)
		tp := SetupPeerPair(t, false)
		requireResilienceReady(t, tp)
		tp.Alice.HealthMonitor.MaxFailures = 1
		tp.Bob.HealthMonitor.MaxFailures = 1
		watcher := watchReconnect(tp)

		// Baseline: the pre-reconnect session carries data.
		rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir,
			"pre-reconnect.bin", []byte("baseline transfer before the forced reconnect"))

		at := uxlatForcedReconnect(t, tp, watcher)
		// Recovery: reconnected event -> first successful post-reconnect round trip.
		rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir,
			"post-reconnect.bin", []byte("data after the reconnect proves the rotated session carries bytes"))
		recovery := time.Since(at)

		require.Lessf(t, recovery, 10*time.Second, "NoFUSE reconnect recovery %s too slow", recovery)
		t.Logf("NoFUSE reconnect recovery (reconnected-event -> first data): %s", recovery)
	})

	t.Run("FUSE", func(t *testing.T) {
		skipIfNoFUSE(t)
		disableKeyUpdate(t)
		tp := SetupFUSEPeerPair(t, 90*time.Second)
		waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
		requireResilienceReady(t, tp)
		tp.Alice.HealthMonitor.MaxFailures = 1
		tp.Bob.HealthMonitor.MaxFailures = 1
		watcher := watchReconnect(tp)

		// Baseline: Bob (no-FUSE) shares, Alice sees it materialize on her mount.
		rekeyRoundTripToMount(t, tp.Bob, tp.BobSaveDir, tp.AliceMountDir,
			"pre-reconnect.txt", []byte("baseline mount receive before the forced reconnect"))

		at := uxlatForcedReconnect(t, tp, watcher)
		rekeyRoundTripToMount(t, tp.Bob, tp.BobSaveDir, tp.AliceMountDir,
			"post-reconnect.txt", []byte("data after the reconnect materializes on the FUSE mount"))
		recovery := time.Since(at)

		require.Lessf(t, recovery, 10*time.Second, "FUSE reconnect recovery %s too slow", recovery)
		t.Logf("FUSE reconnect recovery (reconnected-event -> first mount materialize): %s", recovery)
	})
}

// TestUXLatency_InteractiveFirstByte measures the latency to open and read the first block of
// many small files with the ratchet forced versus a baseline, asserting the forced median stays
// within noise. Baseline and forced run as separate subtests so each threshold override is torn
// down before the next phase.
func TestUXLatency_InteractiveFirstByte(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping interactive-first-byte latency in short mode")
	}
	const nFiles = 40

	run := func(t *testing.T, fuse bool) {
		var baseLat, forcedLat []time.Duration
		var forcedBumps int
		okBase := t.Run("baseline", func(t *testing.T) { baseLat, _, _ = uxlatSyncSmallFiles(t, fuse, nFiles, false) })
		okForced := t.Run("forced", func(t *testing.T) { forcedLat, _, forcedBumps = uxlatSyncSmallFiles(t, fuse, nFiles, true) })
		if !okBase || !okForced {
			t.Fatal("a phase subtest failed; skipping the cross-phase comparison (its inputs are zero values)")
		}

		medBase := durPercentile(baseLat, 0.50)
		medForced := durPercentile(forcedLat, 0.50)
		p90Base := durPercentile(baseLat, 0.90)
		p90Forced := durPercentile(forcedLat, 0.90)
		t.Logf("first-byte over %d files: baseline p50=%s p90=%s, forced p50=%s p90=%s (%d ratchets)",
			nFiles, medBase, p90Base, medForced, p90Forced, forcedBumps)

		require.GreaterOrEqualf(t, forcedBumps, uxlatMinBumps, "forced rekey must actually rotate, got %d bumps", forcedBumps)
		// The forced median must not exceed 2x baseline (the ratchet is a local microsecond KDF).
		// The +5ms absolute slack keeps sub-millisecond loopback medians from flaking on jitter.
		require.LessOrEqualf(t, medForced, 2*medBase+5*time.Millisecond,
			"forced-rekey first-byte median regressed: forced p50=%s vs baseline p50=%s", medForced, medBase)
	}

	t.Run("NoFUSE", func(t *testing.T) { run(t, false) })
	t.Run("FUSE", func(t *testing.T) { skipIfNoFUSE(t); run(t, true) })
}

// TestUXLatency_ManySmallFiles syncs a burst of many small files with the ratchet forced versus
// a baseline and asserts the total transfer time stays within noise while confirming rekeys
// fired: a per-rotation stall or forced flush would inflate the forced total across a large N.
func TestUXLatency_ManySmallFiles(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping many-small-files latency in short mode")
	}

	run := func(t *testing.T, fuse bool, nFiles int) {
		// Two runs per phase, compared on the MIN: the metric is one wall-clock total over
		// hundreds of sequential loopback pulls, and a single scheduler/GC stall swings it by
		// ~2x. A real per-rotation cost inflates every run and survives the min; one-off noise
		// does not.
		var baseTotal, forcedTotal time.Duration
		var forcedBumps int
		minPhase := func(name string, forced bool) (time.Duration, int, bool) {
			best := time.Duration(0)
			bumps := 0
			for i := 1; i <= 2; i++ {
				var total time.Duration
				var b int
				ok := t.Run(fmt.Sprintf("%s-%d", name, i), func(t *testing.T) {
					_, total, b = uxlatSyncSmallFiles(t, fuse, nFiles, forced)
				})
				if !ok {
					return 0, 0, false
				}
				if best == 0 || total < best {
					best, bumps = total, b
				}
			}
			return best, bumps, true
		}
		var okBase, okForced bool
		baseTotal, _, okBase = minPhase("baseline", false)
		forcedTotal, forcedBumps, okForced = minPhase("forced", true)
		if !okBase || !okForced {
			t.Fatal("a phase subtest failed; skipping the cross-phase comparison (its inputs are zero values)")
		}

		t.Logf("burst of %d files: baseline total=%s, forced total=%s (%d ratchets), ratio forced/base=%.2f",
			nFiles, baseTotal, forcedTotal, forcedBumps, float64(forcedTotal)/float64(baseTotal))

		require.GreaterOrEqualf(t, forcedBumps, uxlatMinBumps, "forced rekey must actually rotate, got %d bumps", forcedBumps)
		// Aggregate over a large N is stable, so a tighter 1.5x bound holds; the +50ms slack
		// absorbs one-off setup jitter without letting a real regression through.
		require.LessOrEqualf(t, forcedTotal, 3*baseTotal/2+50*time.Millisecond,
			"forced-rekey burst total regressed: forced=%s vs baseline=%s", forcedTotal, baseTotal)
	}

	// FUSE mount ops are heavier per file, so it uses a smaller burst; non-FUSE runs the full burst.
	t.Run("NoFUSE", func(t *testing.T) { run(t, false, 300) })
	t.Run("FUSE", func(t *testing.T) { skipIfNoFUSE(t); run(t, true, 150) })
}

// TestUXLatency_LiveEditPropagation repeatedly edits one file in place on the sharer (each
// revision the same byte length) with the ratchet forced versus a baseline, and asserts the
// edit-to-visible latency stays within noise while confirming many rekeys fired. It uses the
// non-FUSE re-share path (as uxlatEditPropagation documents), exercising the identical ratchet.
func TestUXLatency_LiveEditPropagation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-edit-propagation latency in short mode")
	}
	const nEdits = 40

	var baseLat, forcedLat []time.Duration
	var forcedBumps int
	okBase := t.Run("baseline", func(t *testing.T) { baseLat, _ = uxlatEditPropagation(t, nEdits, false) })
	okForced := t.Run("forced", func(t *testing.T) { forcedLat, forcedBumps = uxlatEditPropagation(t, nEdits, true) })
	if !okBase || !okForced {
		t.Fatal("a phase subtest failed; skipping the cross-phase comparison (its inputs are zero values)")
	}

	medBase := durPercentile(baseLat, 0.50)
	medForced := durPercentile(forcedLat, 0.50)
	p90Base := durPercentile(baseLat, 0.90)
	p90Forced := durPercentile(forcedLat, 0.90)
	t.Logf("edit-to-visible over %d in-place edits: baseline p50=%s p90=%s, forced p50=%s p90=%s (%d ratchets)",
		nEdits, medBase, p90Base, medForced, p90Forced, forcedBumps)

	require.GreaterOrEqualf(t, forcedBumps, uxlatMinBumps, "forced rekey must actually rotate, got %d bumps", forcedBumps)
	require.LessOrEqualf(t, medForced, 2*medBase+5*time.Millisecond,
		"forced-rekey edit-to-visible median regressed: forced p50=%s vs baseline p50=%s", medForced, medBase)
}
