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
	"os"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestConnect_TimingGap_JoinerAfterCreatorP2PTimeout reproduces issue #146:
// the creator registers and times out on P2P (15s), then the joiner starts.
// Before the fix, the joiner wasted 15s on a phantom P2P connection to the
// creator's stale listener, then both peers got stuck.
// After the fix, the joiner's P2P dial is refused and both fall back to bridge.
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

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(handler)

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
	aliceReady := make(chan error, 1)
	go func() { aliceReady <- kdAlice.CreateRoom() }()

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
	bobReady := make(chan error, 1)
	go func() { bobReady <- kdBob.JoinRoom() }()

	// Both should connect via bridge within 30s.
	select {
	case err := <-aliceReady:
		require.NoError(err, "Alice CreateRoom failed")
		t.Logf("Alice connected (mode: %s)", kdAlice.ConnectionMode)
	case <-ctx.Done():
		t.Fatal("timeout waiting for Alice CreateRoom")
	}

	select {
	case err := <-bobReady:
		require.NoError(err, "Bob JoinRoom failed")
	case <-ctx.Done():
		t.Fatal("timeout waiting for Bob JoinRoom — joiner stuck (issue #146)")
	}

	bobDuration := time.Since(bobStart)
	t.Logf("Bob connected in %s (mode: %s)", bobDuration, kdBob.ConnectionMode)

	require.Equal("bridge", kdAlice.ConnectionMode, "Alice should be on bridge")
	require.Equal("bridge", kdBob.ConnectionMode, "Bob should be on bridge")

	// Cleanup
	kdAlice.StopConnectionResilience()
	kdBob.StopConnectionResilience()
	kdAlice.Shutdown()
	kdBob.Shutdown()
	done := make(chan struct{})
	go func() { runWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	relay.Close()
	bridge.Close()
	cancel()
}
