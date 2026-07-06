// ABOUTME: Repro proving the concurrency review's teardown data race is real: the notify
// ABOUTME: worker flush reads kd.session lock-free while Run's teardown rewrites it under kd.mu.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// TestSessionTeardownRacesNotifyFlush proves the F2 hardening is incomplete on the
// reader side. It drives the REAL notify worker, whose flush closure reads kd.session
// lock-free (the "kd.session == nil" nil-check and the "client := kd.session.GRPCClient"
// deref), against a writer that rewrites kd.session UNDER kd.mu, exactly as Run's
// teardown does when it nils the session. Because flush never takes kd.mu, the mutex on
// the writer establishes no happens-before with the read, so the two collide with no
// synchronization: a data race the -race detector fires on. Locking only the writer (and
// onRekeyNeeded) therefore does not make the notify path race-clean.
//
// Run: go test -race -run TestSessionTeardownRacesNotifyFlush -count=5 ./pkg/logic/common/
func TestSessionTeardownRacesNotifyFlush(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "1")
	kd := newTestKD(t)

	// A non-blocking no-op gRPC client so flush passes its nil-check and reaches the
	// second lock-free read (kd.session.GRPCClient) without doing real network I/O.
	rel := make(chan struct{})
	close(rel)
	fake := &blockingNotifyClient{entered: make(chan struct{}, 1), release: rel}
	kd.session.GRPCClient = fake

	// Two valid sessions to swap between, so the writer models the teardown's kd.session
	// pointer write without ever nil-ing it: a clean data-race signal, no nil-deref noise.
	sessA := kd.session
	sessB := &session.Session{GRPCClient: fake}

	// Start the real notify worker (creates kd.FS and the flush goroutine).
	if err := kd.setupFilesystem(kd.logger, nil); err != nil {
		t.Fatalf("setupFilesystem: %v", err)
	}

	var start, done sync.WaitGroup
	start.Add(1)

	const iters = 20000

	// Reader: feed REMOVE notifies so the real flush runs and reads kd.session.
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.FS.OnLocalChange(types.FileEvent{Action: types.RemoveFile, Path: "gone.txt"})
		}
	}()

	// Writer: rewrite kd.session under kd.mu, mirroring Run's teardown write.
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.mu.Lock()
			if kd.session == sessA {
				kd.session = sessB
			} else {
				kd.session = sessA
			}
			kd.mu.Unlock()
		}
	}()

	start.Done()
	done.Wait()
}
