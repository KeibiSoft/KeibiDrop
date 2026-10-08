// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: The creator's half of the local-mode key exchange: accept on the listener
// ABOUTME: until a joiner's keys arrive, dropping whatever else the backlog holds.

package common

import (
	"log/slog"
	"net"
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
	// A cancel ends the wait. Accept does not watch the connect, so a cancelled
	// create stayed in Accept, kept the connect slot ("did not unwind in time")
	// and took the next create's joiner (2026-10-08, local mode).
	abort := kd.connectAbortDone()
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
		stop := endAcceptOnAbort(ln, abort)
		keyConn, err := ln.Accept()
		stop()
		if kd.connectAbortRequested() || abortFired(abort) {
			if keyConn != nil {
				keyConn.Close()
			}
			return ErrConnectCancelled
		}
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

// endAcceptOnAbort moves the listener's deadline to now when abort fires, so an
// Accept waiting on it returns, as directAcceptor.stop does. stop ends the watch
// and, if the deadline was moved, clears it again: the listener stays open for
// the next connect.
func endAcceptOnAbort(ln net.Listener, abort <-chan struct{}) (stop func()) {
	dl, ok := ln.(deadlineListener)
	if !ok || abort == nil {
		return func() {}
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	moved := false
	go func() {
		defer close(done)
		select {
		case <-abort:
			_ = dl.SetDeadline(time.Now())
			moved = true
		case <-quit:
		}
	}()
	return func() {
		close(quit)
		<-done
		if moved {
			_ = dl.SetDeadline(time.Time{})
		}
	}
}

// abortFired reports that the abort channel of the connect in flight closed.
// A nil channel (no connect in flight) never fires.
func abortFired(abort <-chan struct{}) bool {
	select {
	case <-abort:
		return true
	default:
		return false
	}
}
