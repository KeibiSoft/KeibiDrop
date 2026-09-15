// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
)

// errSessionSocketConsumed is what the gRPC client's dialer answers once the
// session's outbound socket has been handed over.
var errSessionSocketConsumed = errors.New("session outbound socket already consumed; the peer link is down until a reconnect rebuilds it")

// oneShotDialer hands the session's outbound socket to grpc-go once. The socket
// IS the transport, so there is no address to dial again: grpc-go re-invokes the
// dialer whenever its HTTP/2 connection is lost, and handing back the same
// socket, closed by then, made every attempt report "failed to write client
// preface: use of closed network connection", which read like a bridge fault
// (cold install run 1, 2026-09-15). The error names the state instead, so the
// health monitor's misses read as the loss they are.
func oneShotDialer(c net.Conn) func(context.Context, string) (net.Conn, error) {
	var used atomic.Bool
	return func(context.Context, string) (net.Conn, error) {
		if used.Swap(true) {
			return nil, errSessionSocketConsumed
		}
		return c, nil
	}
}
