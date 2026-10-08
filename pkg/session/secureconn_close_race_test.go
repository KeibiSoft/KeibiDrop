// ABOUTME: Regression race test for concurrent SecureConn.Close.
// ABOUTME: The proactive rekey drop and gRPC's transport both close the same conn.

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package session

import (
	"net"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// Two owners (the proactive-rekey socket drop and gRPC's transport) close the same *SecureConn
// concurrently. Both pass the check-then-act on the non-atomic `done` (data race) and both run
// close(s.closed) (panic). Start-barrier + high iteration make the window deterministic under -race.
func TestSecureConnConcurrentClose(t *testing.T) {
	key := testkit.RandBytes(t, 32)
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
