// ABOUTME: Race detector test for the kd.ctx bare-read hazard found by concurrency review:
// ABOUTME: Run's reconnect branch swaps kd.ctx under kd.mu, but several methods read it bare.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"sync"
	"testing"

	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

// TestNotifyRestoredFilesRacesCtxReconnect reproduces the kd.ctx finding: Run's reconnect
// branch (types.go) swaps kd.ctx and kd.Cancel under kd.mu, but notifyRestoredFiles reads
// kd.ctx with no lock. PullFile, PullFileWithParams, JoinRoom, and registerRoomToRelay
// share the exact same unguarded read; notifyRestoredFiles is the smallest real call site
// to drive without a full GRPC/session/relay-HTTP harness. -race must fire with no fix
// applied.
func TestNotifyRestoredFilesRacesCtxReconnect(t *testing.T) {
	kd := newTestKD(t)

	// Non-blocking fake client so notifyRestoredFiles passes its nil-check and reaches the
	// bare kd.ctx reads below without real network I/O.
	rel := make(chan struct{})
	close(rel)
	kd.KDClient = &blockingNotifyClient{entered: make(chan struct{}, 1), release: rel}

	// One LocalFiles entry so the loop body (and its kd.ctx reads) actually runs each call.
	// The path need not exist: os.Stat failing just continues the loop after the kd.ctx.Err()
	// check has already executed.
	kd.SyncTracker.LocalFilesMu.Lock()
	kd.SyncTracker.LocalFiles["race-file"] = &synctracker.File{
		Name:           "race-file",
		RelativePath:   "race-file",
		RealPathOfFile: "/nonexistent/race-file",
	}
	kd.SyncTracker.LocalFilesMu.Unlock()

	var start, done sync.WaitGroup
	start.Add(1)

	const iters = 5000

	// Reader: the real notifyRestoredFiles, which bare-reads kd.ctx.
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.notifyRestoredFiles(kd.logger)
		}
	}()

	// Writer: Run's reconnect-branch ctx swap (types.go), under kd.mu.
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.mu.Lock()
			kd.ctx, kd.Cancel = context.WithCancel(context.Background())
			kd.mu.Unlock()
		}
	}()

	start.Done()
	done.Wait()
}
