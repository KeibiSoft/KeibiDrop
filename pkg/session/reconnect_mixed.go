// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Reconnects a session whose joiner leg is direct and whose creator leg rides
// ABOUTME: the bridge, with the same mapping a fresh create and join produce.

package session

import (
	"fmt"
	"log/slog"
	"time"
)

// Transports a reconnect attempt can use; LastTransport reports the one that worked.
const (
	transportDirect = "direct"
	transportBridge = "bridge"
	transportMixed  = "mixed"
)

// reconnectMixed rebuilds the mixed shape. The responder (the higher fingerprint, the
// joiner of a fresh connect) dials the initiator directly for its outbound leg, then
// takes its inbound from the bridge on pair2. The initiator (the creator of a fresh
// connect, the reachable side) accepts that direct leg, then dials pair2 for its own
// outbound. This is exactly what a fresh create meets when a mixed joiner arrives, so
// a peer that restarted mid-outage and came back through CreateRoom pairs with a
// reconnecting joiner, and the other way round (the 2026-09-06 lesson: one mapping
// for fresh and reconnect, or both sides read the same room and wait each other out).
func (r *ReconnectManager) reconnectMixed(logger *slog.Logger, initiator bool) error {
	logger.Info("Reconnecting with a direct joiner leg and a relayed creator leg", "addr", r.BridgeAddr)

	if initiator {
		if r.AcceptConn == nil {
			return fmt.Errorf("no accept function for the direct leg")
		}
		inConn, err := r.AcceptConn(30 * time.Second)
		if err != nil {
			return fmt.Errorf("accept direct leg: %w", err)
		}
		if err := PerformInboundHandshake(r.session, inConn); err != nil {
			_ = inConn.Close()
			return fmt.Errorf("direct inbound handshake: %w", err)
		}

		outConn, err := r.DialBridge("pair2")
		if err != nil {
			return fmt.Errorf("bridge dial (outbound): %w", err)
		}
		if err := PerformOutboundHandshakeOnConn(r.session, outConn); err != nil {
			_ = outConn.Close()
			return fmt.Errorf("bridge outbound handshake: %w", err)
		}
		logger.Info("Reconnected: direct inbound, bridged outbound (initiator)")
		return nil
	}

	if err := r.dialPeerDirect(logger); err != nil {
		return fmt.Errorf("direct outbound: %w", err)
	}
	inConn, err := r.DialBridge("pair2")
	if err != nil {
		return fmt.Errorf("bridge dial (inbound): %w", err)
	}
	if err := PerformInboundHandshakeWait(r.session, inConn, inboundHandshakeTimeout); err != nil {
		_ = inConn.Close()
		return fmt.Errorf("bridge inbound handshake: %w", err)
	}
	logger.Info("Reconnected: direct outbound, bridged inbound (responder)")
	return nil
}
