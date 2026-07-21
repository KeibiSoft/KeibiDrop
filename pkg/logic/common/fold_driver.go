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

// eagerFoldTimeout bounds the fold RPC so a stuck round never leaks a goroutine or pins the
// single-flight guard. The fold is best-effort, so a timeout just leaves the session on the
// handshake key until the next reconnect re-folds.
const eagerFoldTimeout = 15 * time.Second

// eagerFoldEligible reports whether this peer should drive an eager fold: it is the
// deterministic fold initiator and the in-band ratchet is active (a fold applies only through
// the ratchet, so on a non-ratchet session staging would be a silent no-op). Pure over the
// snapshot, so the gating is easy to test without a live connection.
func eagerFoldEligible(sess *session.Session) bool {
	return sess != nil && sess.IsFoldInitiator() && sess.UseKeyUpdate()
}

// maybeStartEagerFold runs one best-effort ephemeral-KEM fold when this peer is the initiator
// and the ratchet is on. It snapshots kd.session under kd.mu (mirroring onRekeyNeeded, so a
// concurrent teardown niling kd.session cannot panic us), enforces single-flight, and folds
// in a goroutine so the caller (finishConnect / onReconnected) is never blocked on the round
// trip. The responder answers through the Rekey handler; no driver action is needed there.
func (kd *KeibiDrop) maybeStartEagerFold() {
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if !eagerFoldEligible(sess) {
		return
	}
	if !kd.eagerFoldInFlight.CompareAndSwap(false, true) {
		return // a fold round is already in flight; one at a time
	}
	go kd.runEagerFold(sess)
}

// runEagerFold performs the initiator's fold round and always clears the single-flight guard.
// Best-effort: any failure (RPC error, not-ready session) leaves the session on the handshake
// key until a reconnect re-folds, so it is logged, not fatal.
func (kd *KeibiDrop) runEagerFold(sess *session.Session) {
	defer kd.eagerFoldInFlight.Store(false)
	logger := kd.logger.With("event", "fold")

	ctx, cancel := context.WithTimeout(context.Background(), eagerFoldTimeout)
	defer cancel()

	// Lane failover, not duplication: ONE fold round at a time, tried over the QUIC
	// channel first (isolated from TCP bulk, and it proves the lane end to end), then
	// the TCP client with the remaining budget. Failover is safe — a half-completed
	// QUIC round is superseded by the TCP retry's fresh round (staging is supersede-
	// idempotent). Duplicating one round over both lanes would NOT be safe: KEM
	// encapsulation is randomized, so duplicate requests derive different secrets.
	// Fold errors do not demote the QUIC channel: they can be protocol-level
	// (role/readiness), and the metadata path's own traffic polices channel health.
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

// ErrNoQUICForFold is the sentinel that routes the eager fold to TCP when no QUIC
// channel is up; it never escapes runEagerFold.
var ErrNoQUICForFold = errors.New("no quic channel for fold")
