// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: The creator's half of the local-mode key exchange: accept on the listener
// ABOUTME: until a joiner's keys arrive, dropping whatever else the backlog holds.

package common

import (
	"log/slog"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// localKeyExchangeWait bounds the read of one accepted connection's keys. The
// joiner writes its keys the moment it connects, so a connection that stays
// silent this long is not the joiner. A variable so the test can shrink it.
var localKeyExchangeWait = 10 * time.Second

// acceptLocalKeyExchange accepts on the listener until one connection completes
// the plaintext key exchange. The listener is open from process start, so the
// backlog can hold what reached it before this create: a joiner's handshake dial
// that an earlier attempt never took (2026-09-15: its length prefix decoded as
// "invalid character '\x00'" and the create failed), a scanner, a socket already
// closed. Each such connection is dropped and the next one accepted. The wait for
// the joiner itself has no bound, as before: the other person may click minutes
// later.
func (kd *KeibiDrop) acceptLocalKeyExchange(logger *slog.Logger) error {
	for {
		// Snapshot under kd.mu: a prior bridge-fallback timeout or a concurrent
		// Shutdown can leave the listener nil here. Accept on a nil interface panics.
		kd.mu.Lock()
		ln := kd.listener
		kd.mu.Unlock()
		if ln == nil {
			logger.Warn("Listener not open for local key exchange")
			return ErrListenerNotOpen
		}
		keyConn, err := ln.Accept()
		if err != nil {
			logger.Error("Failed to accept key exchange connection", "error", err)
			return err
		}
		_ = keyConn.SetReadDeadline(time.Now().Add(localKeyExchangeWait))
		err = session.ExchangePublicKeysLocal(kd.session, keyConn, false)
		keyConn.Close()
		if err == nil {
			logger.Info("Local key exchange complete (create side)")
			return nil
		}
		if kd.connectCancelled.Load() {
			return ErrConnectCancelled
		}
		logger.Info("Local key exchange failed on this connection, accepting again", "error", err)
	}
}
