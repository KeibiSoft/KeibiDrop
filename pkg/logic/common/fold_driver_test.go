// ABOUTME: Tests for the eager-fold driver gating and single-flight guard: only the
// ABOUTME: deterministic initiator on a ratchet-active session folds, and never twice at once.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"io"
	"log/slog"
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
}

func TestMaybeStartEagerFold_ClearsGuardWhenRoundFails(t *testing.T) {
	kd := &KeibiDrop{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		session: &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb", PeerSupportsKeyUpdate: true},
	}
	// No GRPCClient, so InitiateFold fails fast; the guard must clear so a reconnect re-folds.
	kd.maybeStartEagerFold()
	require.Eventually(t, func() bool { return !kd.eagerFoldInFlight.Load() },
		2*time.Second, 5*time.Millisecond, "the single-flight guard clears after the round finishes")
}
