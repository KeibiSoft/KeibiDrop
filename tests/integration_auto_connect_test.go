// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration test for auto_connect_peer — two peers with saved
// ABOUTME: contacts establish a session on their own, no manual room calls.

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

// TestAutoConnect_BothSidesEstablishSession is the permanent-link demo: both
// peers boot with auto_connect_peer set to a saved contact and nothing else.
// The watchdogs pick roles by fingerprint tiebreak, survive the joiner racing
// the creator's relay registration, and end in a running session.
func TestAutoConnect_BothSidesEstablishSession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping auto-connect integration test in short mode")
	}
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	relay := NewMockRelay()
	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	aIn := getFreePortInRange(t, 26100, 26249)
	aOut := getFreePortInRange(t, 26250, 26399)
	bIn := getFreePortInRange(t, 26400, 26549)
	bOut := getFreePortInRange(t, 26550, 26699)

	alice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		false, relayURL, aIn, aOut, t.TempDir(), t.TempDir(), false, true, "::1")
	require.NoError(err)
	bob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, relayURL, bIn, bOut, t.TempDir(), t.TempDir(), false, true, "::1")
	require.NoError(err)

	require.NoError(alice.EnablePersistentIdentity(t.TempDir(), common.EnableOpts{}))
	require.NoError(bob.EnablePersistentIdentity(t.TempDir(), common.EnableOpts{}))

	aliceFP, err := alice.ExportFingerprint()
	require.NoError(err)
	bobFP, err := bob.ExportFingerprint()
	require.NoError(err)

	require.NoError(alice.AddressBook.Add("bob", bobFP))
	require.NoError(bob.AddressBook.Add("alice", aliceFP))

	var runWg sync.WaitGroup
	runWg.Add(2)
	go func() { defer runWg.Done(); alice.Run() }()
	go func() { defer runWg.Done(); bob.Run() }()

	alice.AutoConnectPeer = "bob"
	bob.AutoConnectPeer = "alice"
	require.NoError(alice.StartAutoConnect(ctx))
	require.NoError(bob.StartAutoConnect(ctx))

	WaitForCondition(t, 90*time.Second, 250*time.Millisecond, func() bool {
		return alice.IsRunning() && bob.IsRunning()
	}, "waiting for auto-connect to establish the session on both sides")

	// Unknown contact still fails clearly even while a session runs.
	bob.AutoConnectPeer = "nobody-saved"
	require.Error(bob.StartAutoConnect(ctx))

	cancel()
	alice.StopConnectionResilience()
	bob.StopConnectionResilience()
	alice.Shutdown()
	bob.Shutdown()

	done := make(chan struct{})
	go func() { runWg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}
