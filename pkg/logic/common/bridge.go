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
// fingerprints. Both peers compute the same token regardless of who is
// the creator vs joiner because the fingerprints are sorted first.
func bridgeRoomToken(ownFingerprint, peerFingerprint string) [32]byte {
	fps := []string{ownFingerprint, peerFingerprint}
	sort.Strings(fps)
	return sha256.Sum256([]byte(fps[0] + fps[1]))
}

// dialBridge connects to the bridge relay and sends the room token.
// The bridge pairs connections with matching tokens.
func (kd *KeibiDrop) dialBridge(logger *slog.Logger) (net.Conn, error) {
	conn, err := session.DialWithStableAddr("tcp", kd.BridgeAddr, 15*time.Second, logger)
	if err != nil {
		return nil, fmt.Errorf("dial bridge: %w", err)
	}

	ownFP := kd.session.OwnFingerprint
	peerFP := kd.session.ExpectedPeerFingerprint
	token := bridgeRoomToken(ownFP, peerFP)

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(token[:]); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send room token: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	logger.Info("Bridge room token sent", "token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	return conn, nil
}
