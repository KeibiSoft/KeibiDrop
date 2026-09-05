// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: A joiner that reaches the bridge right after the creator's empty bridge round
// ABOUTME: must connect; it used to be paired with the creator's closed leg (a corpse room).

package tests

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// The always-on box against a laptop. The creator (open inbound, a fresh
// reachable probe verdict, so it keeps its direct accept windows) cycles
// 15 s accept, 15 s bridge. The bridge keeps an unmatched room after the
// client closed it, like the production DataRelay does for its expiry window,
// and the mock keeps it for good. A joiner whose tokens land in the creator's
// second accept window used to be paired with the leg closed at the end of the
// first bridge round and never met the creator. The creator now parks that leg
// open and reads it first, so the joiner's hello is there when the creator
// reaches the bridge again.
//
// The joiner never dials the creator directly: the creator advertises an
// address the joiner refuses, so the joiner takes the bridge at once and the
// test controls the arrival time exactly.
func TestConnect_JoinerAfterCreatorsEmptyBridgeRound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode (takes about 50s)")
	}
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	relay := NewMockRelay()
	relay.SetProbeReachable(true)
	bridge, err := NewMockBridge()
	require.NoError(err)
	defer relay.Close()
	defer bridge.Close()

	logger := testkit.StdoutLogger(slog.LevelInfo)

	aliceInPort := getFreePortInRange(t, 26100, 26249)
	aliceOutPort := getFreePortInRange(t, 26250, 26399)
	bobInPort := getFreePortInRange(t, 26400, 26549)
	bobOutPort := getFreePortInRange(t, 26550, 26699)

	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	// An IPv4 literal is not an IPv6 address to the joiner, so it never dials
	// the creator directly. The creator itself only needs the field non-empty
	// to keep its direct accept window.
	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		false, relayURL, aliceInPort, aliceOutPort,
		t.TempDir(), t.TempDir(), false, false, "127.0.0.1")
	require.NoError(err)
	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, relayURL, bobInPort, bobOutPort,
		t.TempDir(), t.TempDir(), false, false, "::1")
	require.NoError(err)

	kdAlice.BridgeAddr = bridge.FormatAddr()
	kdBob.BridgeAddr = bridge.FormatAddr()

	aliceFp, err := kdAlice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := kdBob.ExportFingerprint()
	require.NoError(err)
	require.NoError(kdAlice.AddPeerFingerprint(bobFp))
	require.NoError(kdBob.AddPeerFingerprint(aliceFp))

	// The verdict that keeps the creator's accept windows across empty rounds.
	kdAlice.ProbeInboundReachability(ctx)

	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); kdAlice.Run() }()
	go func() { defer runWg.Done(); kdBob.Run() }()

	aliceReady := testkit.Go(func() error { return kdAlice.CreateRoom() })

	// Round 0: 15 s accept window, then the bridge leg appears at the bridge.
	WaitForCondition(t, 30*time.Second, 100*time.Millisecond, func() bool {
		return bridge.PendingCount() == 1
	}, "waiting for the creator's first bridge leg")
	legAt := time.Now()

	// That leg's wait ends 15 s later and the second accept window opens. Land
	// the joiner 3 s into it: the room from the first leg is still at the bridge.
	time.Sleep(18 * time.Second)
	t.Logf("joiner starts %s after the creator's first bridge leg", time.Since(legAt).Round(time.Second))
	bobStart := time.Now()
	bobReady := testkit.Go(func() error { return kdBob.JoinRoom() })

	// The creator reads the parked leg when its window closes, at most 15 s
	// later, then pairs the second leg. 40 s leaves margin; the old code needed
	// the joiner's whole bridge window to fail.
	joinCtx, joinCancel := context.WithTimeout(ctx, 40*time.Second)
	defer joinCancel()
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error {
				return testkit.WithinCtx(joinCtx, "Bob JoinRoom after the creator's empty bridge round", bobReady)
			},
			func() error { return testkit.WithinCtx(joinCtx, "Alice CreateRoom", aliceReady) },
		)
	})
	t.Logf("Bob connected in %s (mode: %s)", time.Since(bobStart).Round(time.Millisecond), kdBob.ConnectionMode)
	require.Equal("bridge", kdAlice.ConnectionMode)
	require.Equal("bridge", kdBob.ConnectionMode)

	kdAlice.StopConnectionResilience()
	kdBob.StopConnectionResilience()
	kdAlice.Shutdown()
	kdBob.Shutdown()
	_ = testkit.Within(5*time.Second, "peer Run goroutines", func() error { runWg.Wait(); return nil })
}
