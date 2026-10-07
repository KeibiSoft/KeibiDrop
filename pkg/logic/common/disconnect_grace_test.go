// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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

	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "NewKeibiDropWithIP failed")

	var cancelAt atomic.Int64
	kd.Cancel = func() { cancelAt.Store(time.Now().UnixNano()) }

	start := time.Now()
	kd.handleNotifyDisconnect()

	got := cancelAt.Load()
	require.NotZero(t, got, "Cancel was never invoked by handleNotifyDisconnect")

	elapsed := time.Unix(0, got).Sub(start)
	require.GreaterOrEqual(t, elapsed, grpcDisconnectGraceDelay,
		"Cancel fired too early (in-flight RPC response would race teardown)")
}
