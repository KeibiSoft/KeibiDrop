// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Regression test for issue #122 — verifies handleNotifyDisconnect
// ABOUTME: waits the grace window before cancelling, so the in-flight
// ABOUTME: DISCONNECT RPC response flushes before grpcServer.Stop().

package common

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// TestHandleNotifyDisconnectHonorsGraceDelay verifies that the post-DISCONNECT
// teardown waits grpcDisconnectGraceDelay before invoking cancelContext. Issue
// #122: cancelling earlier races the in-flight DISCONNECT RPC response write
// and either crashes the gRPC server or leaks Serve()/ClientConn goroutines.
func TestHandleNotifyDisconnectHonorsGraceDelay(t *testing.T) {
	origDelay := grpcDisconnectGraceDelay
	grpcDisconnectGraceDelay = 50 * time.Millisecond
	t.Cleanup(func() { grpcDisconnectGraceDelay = origDelay })

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, 26720, 26721, "", t.TempDir(), false, false, "::1")
	if err != nil {
		t.Fatalf("NewKeibiDropWithIP failed: %v", err)
	}

	var cancelAt atomic.Int64
	kd.Cancel = func() { cancelAt.Store(time.Now().UnixNano()) }

	start := time.Now()
	kd.handleNotifyDisconnect()

	got := cancelAt.Load()
	if got == 0 {
		t.Fatal("Cancel was never invoked by handleNotifyDisconnect")
	}

	elapsed := time.Unix(0, got).Sub(start)
	if elapsed < grpcDisconnectGraceDelay {
		t.Fatalf("Cancel fired too early: %v < grace %v (in-flight RPC response would race teardown)",
			elapsed, grpcDisconnectGraceDelay)
	}
}
