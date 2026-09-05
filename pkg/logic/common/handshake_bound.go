// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Runs an inbound handshake and closes the connection when it fails, so a
// ABOUTME: rejected peer cannot leak the accepted conn.

package common

import (
	"net"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// Indirection so tests can stub the handshake.
var performInboundHandshake = session.PerformInboundHandshake

// handshakeOrClose closes the connection when the handshake fails. A failed handshake
// never reaches the session (the fingerprint check gates that), so closing here is safe.
// The read bound itself lives inside session.PerformInboundHandshake, where no call site
// can forget it.
func handshakeOrClose(s *session.Session, c net.Conn) error {
	if err := performInboundHandshake(s, c); err != nil {
		c.Close()
		return err
	}
	return nil
}

// joinBridgeInbound runs the joiner's inbound handshake on its own bridge leg and
// closes the leg when it fails. The accept-loop bound in handshakeOrClose is too
// short here: the creator may still be dialing our blocked listener. This is a
// socket we dialed, not an accept loop, so the longer window pins nothing a
// stranger can reach.
func (kd *KeibiDrop) joinBridgeInbound(c net.Conn) error {
	if err := session.PerformInboundHandshakeWait(kd.session, c, joinBridgeWait); err != nil {
		c.Close()
		return err
	}
	return nil
}
