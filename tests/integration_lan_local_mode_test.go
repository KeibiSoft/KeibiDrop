// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Two peers in local mode on one network connect over the LAN, whatever the
// ABOUTME: relay's probe said about the creator's public address, whoever clicks first.

package tests

import (
	"context"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// The 2026-09-15 shape: make run-alice and make run-bob on one Mac behind a home
// router. The creator had been on the internet path first, so it carried the
// relay's verdict that its public address is blocked. It applied that verdict to
// the joiner on its own subnet, skipped the direct window, and the joiner's LAN
// dial sat in its backlog while both sides waited. Two people never click at the
// same moment either, so the joiner going first and the creator going first are
// both driven.
func TestConnect_LocalMode_SameLAN(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	cases := []struct {
		name        string
		creatorLead time.Duration // the creator waits alone this long before the joiner clicks
		joinerLead  time.Duration // the joiner's dial waits this long before the creator clicks
	}{
		{"both click at once", 0, 0},
		{"creator clicks first", 700 * time.Millisecond, 0},
		{"joiner clicks first", 0, 700 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			connectLocalModePair(t, tc.creatorLead, tc.joinerLead)
		})
	}
}

func connectLocalModePair(t *testing.T, creatorLead, joinerLead time.Duration) {
	require := require.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	relay := NewMockRelay()
	relay.SetProbeReachable(false) // the home router: nothing reaches the public address
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

	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		false, relayURL, aliceInPort, aliceOutPort,
		t.TempDir(), t.TempDir(), false, false, "::1")
	require.NoError(err)
	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, relayURL, bobInPort, bobOutPort,
		t.TempDir(), t.TempDir(), false, false, "::1")
	require.NoError(err)
	kdAlice.BridgeAddr = bridge.FormatAddr()
	kdBob.BridgeAddr = bridge.FormatAddr()

	// The creator's earlier internet session left the relay's verdict cached.
	kdAlice.ProbeInboundReachability(ctx)
	require.True(kdAlice.InboundBlocked(), "the probe verdict the creator carries into local mode")

	// The LAN switch: discovery hands each side the other's address.
	kdAlice.IsLocalMode = true
	kdBob.IsLocalMode = true
	require.NoError(kdAlice.SetPeerDirectAddress("::1:" + strconv.Itoa(bobInPort)))
	require.NoError(kdBob.SetPeerDirectAddress("::1:" + strconv.Itoa(aliceInPort)))

	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); kdAlice.Run() }()
	go func() { defer runWg.Done(); kdBob.Run() }()

	var aliceReady, bobReady func() error
	if joinerLead > 0 {
		bobReady = testkit.Go(kdBob.JoinRoom)
		time.Sleep(joinerLead)
		aliceReady = testkit.Go(kdAlice.CreateRoom)
	} else {
		aliceReady = testkit.Go(kdAlice.CreateRoom)
		time.Sleep(creatorLead)
		bobReady = testkit.Go(kdBob.JoinRoom)
	}
	began := time.Now()

	// The LAN path is a dial, a handshake and a dial back on loopback. The old
	// code needed the joiner's whole direct window and then never paired on the
	// bridge, so 20 s separates working from broken with room to spare.
	joinCtx, joinCancel := context.WithTimeout(ctx, 20*time.Second)
	defer joinCancel()
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error { return testkit.WithinCtx(joinCtx, "Bob JoinRoom over the LAN", bobReady) },
			func() error { return testkit.WithinCtx(joinCtx, "Alice CreateRoom over the LAN", aliceReady) },
		)
	})
	t.Logf("connected in %s (alice %s, bob %s)", time.Since(began).Round(time.Millisecond),
		kdAlice.ConnectionMode, kdBob.ConnectionMode)
	require.Equal("lan", kdAlice.ConnectionMode, "the creator took the LAN dial, probe verdict or not")
	require.Equal("lan", kdBob.ConnectionMode, "the joiner got the dial back inside its LAN window")

	WaitForCondition(t, 5*time.Second, 10*time.Millisecond, func() bool {
		return kdAlice.IsRunning() && kdBob.IsRunning()
	}, "waiting for both peers to be running")

	kdAlice.StopConnectionResilience()
	kdBob.StopConnectionResilience()
	kdAlice.Shutdown()
	kdBob.Shutdown()
	// A timeout is not a failure here. Teardown proceeds either way.
	_ = testkit.Within(5*time.Second, "peer Run goroutines", func() error { runWg.Wait(); return nil })
}
