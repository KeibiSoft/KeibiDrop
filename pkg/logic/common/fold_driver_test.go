// ABOUTME: Tests for the eager-fold driver gating and single-flight guard: only the
// ABOUTME: deterministic initiator on a ratchet-active session folds, and never twice at once.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

func TestEagerFoldEligible(t *testing.T) {
	require.False(t, eagerFoldEligible(nil), "a nil session is not eligible")

	initiator := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true}
	require.True(t, eagerFoldEligible(initiator), "the lower-fingerprint peer with the ratchet on drives the fold")

	responder := &session.Session{OwnFingerprint: "bbb", ExpectedPeerFingerprint: "aaa", PeerSupportsKeyUpdate: true}
	require.False(t, eagerFoldEligible(responder), "the higher-fingerprint peer answers, it does not initiate")

	noRatchet := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: false}
	require.False(t, eagerFoldEligible(noRatchet), "without the negotiated ratchet a fold cannot apply")
}

func TestMaybeStartEagerFold_SingleFlightGuardBlocksSecond(t *testing.T) {
	kd := &KeibiDrop{session: &session.Session{
		OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true,
	}}
	kd.eagerFoldInFlight.Store(true) // a fold round is already running

	kd.maybeStartEagerFold()
	require.True(t, kd.eagerFoldInFlight.Load(), "a second call is a no-op while a fold is in flight")
	require.True(t, kd.eagerFoldRearm.Load(), "a trigger during an in-flight round rearms a follow-up")
}

func TestRunEagerFold_RearmRunsFollowUpThenClears(t *testing.T) {
	kd := newBareKD()
	kd.session = &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true}
	kd.eagerFoldRearm.Store(true)
	// No GRPCClient: both the round and its rearmed follow-up fail fast.
	kd.maybeStartEagerFold()
	require.Eventually(t, func() bool {
		return !kd.eagerFoldInFlight.Load() && !kd.eagerFoldRearm.Load()
	}, 2*time.Second, 5*time.Millisecond, "the follow-up consumes the rearm and both guards clear")
}

func TestMaybeStartEagerFoldSoon_CoalescesIntoOneTimer(t *testing.T) {
	kd := newBareKD()
	kd.session = &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true}
	kd.maybeStartEagerFoldSoon()
	kd.maybeStartEagerFoldSoon()
	kd.maybeStartEagerFoldSoon()
	require.True(t, kd.eagerFoldScheduled.Load(), "a burst of arrivals arms exactly one timer")
	require.False(t, kd.eagerFoldInFlight.Load(), "nothing folds before the coalesce delay")
	require.Eventually(t, func() bool {
		return !kd.eagerFoldScheduled.Load() && !kd.eagerFoldInFlight.Load()
	}, 3*time.Second, 10*time.Millisecond, "the deferred round runs once and clears")
}

// A trigger whose single-flight CAS fails publishes its rearm as a separate atomic. If the
// in-flight round retires entirely in the gap between the failed CAS and that publish, the
// rearm lands with no consumer (no round, no timer) and the trigger is lost. The reclaim path
// must notice the round is gone and fold anyway. Not -race-detectable (all atomics), so this
// wedges the interleaving through the eagerFoldRearmGate seam.
func TestMaybeStartEagerFold_ReclaimsRearmWhenRoundRetiresMidPublish(t *testing.T) {
	kd := newBareKD()
	kd.session = &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true}
	kd.eagerFoldInFlight.Store(true) // a round R is in flight, so the trigger's CAS fails

	parked := make(chan struct{}, 1) // buffered: the gate signals once even before we receive
	release := make(chan struct{})
	origGate := eagerFoldRearmGate
	eagerFoldRearmGate = func() {
		select {
		case parked <- struct{}{}:
		default:
		}
		<-release
	}
	t.Cleanup(func() { eagerFoldRearmGate = origGate })

	done := make(chan struct{})
	go func() { kd.maybeStartEagerFold(); close(done) }()
	<-parked // the trigger has failed its CAS and is holding before it publishes the rearm

	// R retires exactly as runEagerFold's defers do: clear single-flight, then find no rearm.
	kd.eagerFoldInFlight.Store(false)
	require.False(t, kd.eagerFoldRearm.Swap(false), "R's retire finds no rearm to consume yet")

	close(release) // the trigger now publishes its rearm into a world with no round in flight
	<-done         // and fully returns; without the reclaim the rearm is stranded true here

	require.Eventually(t, func() bool {
		return !kd.eagerFoldRearm.Load() && !kd.eagerFoldInFlight.Load()
	}, 2*time.Second, 5*time.Millisecond,
		"the trigger reclaims its parked rearm, folds once, and both guards clear")
}

func TestMaybeStartEagerFold_ClearsGuardWhenRoundFails(t *testing.T) {
	kd := newBareKD()
	kd.session = &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true}
	// No GRPCClient, so InitiateFold fails fast; the guard must clear so a reconnect re-folds.
	kd.maybeStartEagerFold()
	require.Eventually(t, func() bool { return !kd.eagerFoldInFlight.Load() },
		2*time.Second, 5*time.Millisecond, "the single-flight guard clears after the round finishes")
}
