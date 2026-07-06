// ABOUTME: Regression race test for concurrent SecureConn.Close (F1).
// ABOUTME: The proactive rekey drop and gRPC's transport both close the same conn.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"net"
	"sync"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// TestSecureConnConcurrentClose reproduces F1: SecureConn.Close is a check-then-act
// on a non-atomic `done` bool plus close(s.closed). When the proactive rekey drops the
// sockets (resilience.onRekeyNeeded) while gRPC's transport still owns the same
// *SecureConn, both close it: two closers pass the `if s.done` check, both write `done`
// (a data race) and both run close(s.closed) (panic: close of closed channel).
// Start-barrier + high iteration make the tiny window deterministic under -race.
func TestSecureConnConcurrentClose(t *testing.T) {
	key := randomKey(t)
	const iters = 300
	for i := 0; i < iters; i++ {
		c1, c2 := net.Pipe()
		sc := NewSecureConn(c1, key, kbc.CipherChaCha20, NoncePrefixOutbound)

		var start, done sync.WaitGroup
		start.Add(1)
		const closers = 3
		done.Add(closers)
		for g := 0; g < closers; g++ {
			go func() {
				defer done.Done()
				start.Wait()
				_ = sc.Close()
			}()
		}
		start.Done() // release all closers at once
		done.Wait()
		_ = c2.Close()
	}
}
