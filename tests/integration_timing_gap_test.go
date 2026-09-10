// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration test for issue #146 — timing gap between creator and joiner.
// ABOUTME: Proves the joiner is not stuck when connecting after the creator's P2P timeout.

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

// TestConnect_TimingGap_JoinerAfterCreatorP2PTimeout reproduces issue #146:
// the creator registers and its first direct window (15s) closes with nobody in
// it, then the joiner starts. Originally the joiner wasted 15s on a phantom P2P
// connection to the creator's stale listener and both peers got stuck; the
// first fix refused the dial and sent both to the bridge for the whole session.
// Since BUGS 33 (2026-09-09) the creator's listener stays open between rounds
// and an empty window marks nothing, so the late joiner connects direct, fast.
//
// This test uses explicit CreateRoom/JoinRoom (not Connect) to control which
// peer is creator and which is joiner, avoiding fingerprint tiebreak randomness.
func TestConnect_TimingGap_JoinerAfterCreatorP2PTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timing gap test in short mode (takes ~20s)")
	}
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)

	relay := NewMockRelay()
	bridge, err := NewMockBridge()
	require.NoError(err)

	logger := testkit.StdoutLogger(slog.LevelInfo)

	aliceInPort := getFreePortInRange(t, 26100, 26249)
	aliceOutPort := getFreePortInRange(t, 26250, 26399)
	bobInPort := getFreePortInRange(t, 26400, 26549)
	bobOutPort := getFreePortInRange(t, 26550, 26699)

	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		false, relayURL, aliceInPort, aliceOutPort,
		t.TempDir(), t.TempDir(), false, false, "::1")
	require.NoError(err)

	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, relayURL, bobInPort, bobOutPort,
		t.TempDir(), t.TempDir(), false, false, "::1")
	require.NoError(err)

	// Set bridge on both peers so they can fall back.
	kdAlice.BridgeAddr = bridge.FormatAddr()
	kdBob.BridgeAddr = bridge.FormatAddr()

	aliceFp, err := kdAlice.ExportFingerprint()
	require.NoError(err)
	bobFp, err := kdBob.ExportFingerprint()
	require.NoError(err)

	require.NoError(kdAlice.AddPeerFingerprint(bobFp))
	require.NoError(kdBob.AddPeerFingerprint(aliceFp))

	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); kdAlice.Run() }()
	go func() { defer runWg.Done(); kdBob.Run() }()

	// Step 1: Alice is the creator (explicit role, no tiebreak randomness).
	aliceReady := testkit.Go(func() error { return kdAlice.CreateRoom() })

	// Wait for Alice to register on relay.
	WaitForCondition(t, 10*time.Second, 50*time.Millisecond, func() bool {
		return relay.EntryCount() > 0
	}, "waiting for Alice to register on relay")

	// Step 2: Wait for creator's 15s P2P accept to time out.
	t.Log("Waiting 18s for creator's P2P timeout and bridge fallback...")
	time.Sleep(18 * time.Second)

	// Step 3: NOW start joiner. This is the timing gap scenario from #146.
	t.Log("Starting joiner (Bob) after creator's P2P timeout...")
	bobStart := time.Now()
	bobReady := testkit.Go(func() error { return kdBob.JoinRoom() })

	// Both connect direct: the creator's next round takes the dial.
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error {
				if err := testkit.WithinCtx(ctx, "Alice CreateRoom", aliceReady); err != nil {
					return err
				}
				t.Logf("Alice connected (mode: %s)", kdAlice.ConnectionMode)
				return nil
			},
			func() error {
				return testkit.WithinCtx(ctx, "Bob JoinRoom, joiner stuck (issue #146)", bobReady)
			},
		)
	})

	bobDuration := time.Since(bobStart)
	t.Logf("Bob connected in %s (mode: %s)", bobDuration, kdBob.ConnectionMode)

	require.Equal("direct", kdAlice.ConnectionMode, "the creator keeps accepting after an empty window")
	require.Equal("direct", kdBob.ConnectionMode, "a late joiner is not sent to the bridge")
	require.Less(bobDuration, 10*time.Second, "no phantom dial, no window to wait out")

	// Cleanup
	kdAlice.StopConnectionResilience()
	kdBob.StopConnectionResilience()
	kdAlice.Shutdown()
	kdBob.Shutdown()
	// A timeout is not a failure here. Teardown proceeds either way.
	_ = testkit.Within(5*time.Second, "peer Run goroutines", func() error { runWg.Wait(); return nil })
	relay.Close()
	bridge.Close()
	cancel()
}
