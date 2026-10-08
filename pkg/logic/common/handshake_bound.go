// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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
	// A cancel closes the leg under the wait, so the joiner returns at once
	// instead of at joinBridgeWait, a full minute.
	stop := kd.closeOnAbort(c)
	err := session.PerformInboundHandshakeWait(kd.session, c, joinBridgeWait)
	stop()
	if err != nil {
		c.Close()
		if kd.connectAbortRequested() {
			return ErrConnectCancelled
		}
		return err
	}
	return nil
}
