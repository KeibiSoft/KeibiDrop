// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Regression for the reconnect fold window: a round staged before a conn reinstall
// ABOUTME: must survive the swap, or the first folded bump after reconnect is undecryptable.

package session

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// TestFoldSurvivesConnReinstall reproduces the reconnect-window behind the CI
// "decryption failed at epoch" failures: a fold round stages on the session's conns, the
// reconnect handshake replaces them, the peer (which staged after its own swap) folds the
// round on its fresh writer's first bump. The fresh reader must follow via the session's
// pending-fold custody.
func TestFoldSurvivesConnReinstall(t *testing.T) {
	logger := testkit.DiscardLogger()
	sess := NewSession(logger, "", time.Minute)
	key := testkit.RandBytes(t, 32)
	suite := kbc.CipherChaCha20

	// The pre-reconnect conn pair; the round stages on it (the RespondToFold effect).
	_, oldInbound := newConnPair(t, key, suite)
	sess.installInbound(oldInbound)
	secret := bytes.Repeat([]byte{7}, 32)
	sess.stageFoldBothConns(secret, false)

	// Reconnect: the handshake installs fresh conns (epoch 0) into the same session.
	peerWriter, freshInbound := newConnPair(t, key, suite)
	peerWriter.SetKeyUpdate(true)
	freshInbound.SetKeyUpdate(true)
	sess.installInbound(freshInbound)

	// The peer staged the same round after its own swap, so its fresh writer is armed.
	peerWriter.StageEntropyFold(secret, true)

	done := make(chan error, 1)
	go func() {
		_, err := peerWriter.Write([]byte("after-reconnect"))
		done <- err
	}()
	buf := make([]byte, 64)
	n, err := freshInbound.Read(buf)
	require.NoError(t, err, "fresh reader must follow the folded bump")
	require.Equal(t, "after-reconnect", string(buf[:n]))
	require.NoError(t, <-done)
}

// TestFoldStageInstallOrdering is the barrier regression for the reconnect-window ORDERING
// gap (concurrency review finding 1): a fold round and a fresh-conn install run concurrently
// off a start barrier. Whichever wins, the fresh reader must decrypt the peer's folded bump.
// -race cannot see this (ordering, not a torn read), so the assertion is the oracle; run it
// -count=50+. Pre-fix (remember-after-snapshot or adopt-before-assign) this fails as
// "decryption failed at epoch N".
func TestFoldStageInstallOrdering(t *testing.T) {
	logger := testkit.DiscardLogger()
	key := testkit.RandBytes(t, 32)
	suite := kbc.CipherChaCha20
	secret := bytes.Repeat([]byte{9}, 32)

	for i := 0; i < 200; i++ {
		sess := NewSession(logger, "", time.Minute)
		// A pre-existing conn so stageFoldBothConns has a live lane to stage (its snapshot is
		// non-empty); the fresh install below is the one racing the round.
		_, seed := newConnPair(t, key, suite)
		seed.SetKeyUpdate(true)
		sess.installInbound(seed)

		peerWriter, freshInbound := newConnPair(t, key, suite)
		peerWriter.SetKeyUpdate(true)
		freshInbound.SetKeyUpdate(true)

		var start sync.WaitGroup
		start.Add(1)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); start.Wait(); sess.stageFoldBothConns(secret, false) }()
		go func() { defer wg.Done(); start.Wait(); sess.installOutbound(freshInbound) }()
		start.Done()
		wg.Wait()

		// The peer staged the same round after its own swap; its writer is armed on this lane.
		peerWriter.StageEntropyFold(secret, true)
		done := make(chan error, 1)
		go func() { _, err := peerWriter.Write([]byte("x")); done <- err }()
		buf := make([]byte, 8)
		n, err := freshInbound.Read(buf)
		require.NoErrorf(t, err, "iter %d: fresh reader must follow the folded bump", i)
		require.Equal(t, "x", string(buf[:n]))
		require.NoError(t, <-done)
	}
}
