// ABOUTME: The eager entropy-fold driver: on connect and reconnect the deterministic
// ABOUTME: initiator runs one best-effort ephemeral-KEM fold to make the session forward-secret.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// eagerFoldTimeout bounds the fold RPC. A stuck round must not leak a goroutine or
// pin the single-flight guard. On timeout the session stays on the handshake key.
const eagerFoldTimeout = 15 * time.Second

// foldCoalesceDelay batches near-simultaneous lane-wire arrivals into one fold round.
const foldCoalesceDelay = time.Second

// eagerFoldEligible reports whether this peer must drive an eager fold. It requires
// the fold-initiator role and an active in-band ratchet.
func eagerFoldEligible(sess *session.Session) bool {
	return sess != nil && sess.IsFoldInitiator() && sess.UseKeyUpdate()
}

// eagerFoldRearmGate is a no-op test seam between the failed CAS and the rearm publish.
// Tests wedge the window where the round retires in that gap. Production leaves it empty.
var eagerFoldRearmGate = func() {}

// maybeStartEagerFold runs one best-effort ephemeral-KEM fold on the eligible initiator.
// It snapshots kd.session under kd.mu because teardown nils it concurrently.
// It enforces single-flight and folds in a goroutine, so the caller never blocks.
func (kd *KeibiDrop) maybeStartEagerFold() {
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if !eagerFoldEligible(sess) {
		return
	}
	if !kd.eagerFoldInFlight.CompareAndSwap(false, true) {
		eagerFoldRearmGate()
		kd.eagerFoldRearm.Store(true) // Request one follow-up round so the trigger is not lost.
		// The round can retire between the failed CAS and the store above. Then the rearm
		// has no consumer. Reclaim it and retry; do not park the trigger.
		for !kd.eagerFoldInFlight.Load() {
			if !kd.eagerFoldRearm.Swap(false) {
				return // A retiring round or another trigger took the token and re-entered.
			}
			if kd.eagerFoldInFlight.CompareAndSwap(false, true) {
				go kd.runEagerFold(sess)
				return
			}
			kd.eagerFoldRearm.Store(true) // A new round won the CAS. Republish for it and recheck.
		}
		return
	}
	go kd.runEagerFold(sess)
}

// maybeStartEagerFoldSoon coalesces wire-arrival triggers into one deferred round.
func (kd *KeibiDrop) maybeStartEagerFoldSoon() {
	if !kd.eagerFoldScheduled.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(foldCoalesceDelay, func() {
		kd.eagerFoldScheduled.Store(false)
		kd.maybeStartEagerFold()
	})
}

// runEagerFold performs the initiator's fold round and always clears the single-flight guard.
// On failure the session stays on the handshake key until a reconnect folds again.
func (kd *KeibiDrop) runEagerFold(sess *session.Session) {
	// Defers run LIFO. The guard clears before the rearm re-enters.
	defer func() {
		if kd.eagerFoldRearm.Swap(false) {
			kd.maybeStartEagerFold()
		}
	}()
	defer kd.eagerFoldInFlight.Store(false)
	logger := kd.logger.With("event", "fold")

	ctx, cancel := context.WithTimeout(context.Background(), eagerFoldTimeout)
	defer cancel()

	// One round with lane failover: QUIC first, then TCP with the remaining budget.
	// The TCP retry supersedes a half-completed QUIC round; staging is supersede-idempotent.
	// KEM encapsulation is randomized, so two lanes would derive different secrets.
	// Fold errors do not demote QUIC.
	lane := "tcp"
	err := ErrNoQUICForFold
	if qc := kd.QUICKeibiClient(); qc != nil {
		qctx, qcancel := context.WithTimeout(ctx, quicMetaTimeout)
		err = sess.InitiateFoldVia(qctx, qc)
		qcancel()
		lane = "quic"
	}
	if err != nil {
		if lane == "quic" {
			logger.Debug("Eager fold over QUIC failed; retrying over TCP", "error", err)
		}
		lane = "tcp"
		err = sess.InitiateFold(ctx)
	}
	if err != nil {
		logger.Info("Eager fold did not complete; session stays on the handshake key until reconnect", "error", err)
		return
	}
	logger.Info("Eager fold committed; session is forward-secret against later identity-key theft",
		"epoch", kd.WriterEpoch(), "dir", "initiate", "lane", lane)
}

// ErrNoQUICForFold routes the eager fold to TCP when no QUIC channel is up.
// It never escapes runEagerFold.
var ErrNoQUICForFold = errors.New("no quic channel for fold")
