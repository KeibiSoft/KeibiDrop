// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
package common

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// bridgeRoomToken computes a deterministic 32-byte room token from two
// fingerprints and a direction label. Both peers compute the same token
// for a given direction regardless of who is creator vs joiner.
func bridgeRoomToken(ownFingerprint, peerFingerprint, direction string) [32]byte {
	fps := []string{ownFingerprint, peerFingerprint}
	sort.Strings(fps)
	return sha256.Sum256([]byte(fps[0] + fps[1] + direction))
}

// dialBridgeDir connects to the bridge relay and sends a direction-tagged room token.
// Using separate tokens for "out" and "in" prevents the bridge from pairing
// two connections from the same peer (self-pair bug).
func (kd *KeibiDrop) dialBridgeDir(direction string, logger *slog.Logger) (net.Conn, error) {
	conn, err := session.DialWithStableAddr("tcp", kd.BridgeAddr, 15*time.Second, logger)
	if err != nil {
		return nil, fmt.Errorf("dial bridge: %w", err)
	}

	ownFP := kd.session.OwnFingerprint
	peerFP := kd.session.ExpectedPeerFingerprint
	token := bridgeRoomToken(ownFP, peerFP, direction)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(token[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send room token: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	logger.Info("Bridge room token sent", "dir", direction, "token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	return conn, nil
}
