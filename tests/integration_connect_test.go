// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// SetupPeerPairViaConnect creates two KeibiDrop instances and connects them
// using Connect() (deterministic fingerprint tiebreaker) instead of explicit
// CreateRoom/JoinRoom. Both peers call Connect() simultaneously — the retry
// loop in JoinRoom handles the timing.
func SetupPeerPairViaConnect(t *testing.T, isFuse bool) *TestPair {
	t.Helper()
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)

	relay := NewMockRelay()
	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	aliceSave := t.TempDir()
	bobSave := t.TempDir()
	aliceMount := t.TempDir()
	bobMount := t.TempDir()

	logger := testkit.StdoutLogger(slog.LevelWarn)

	aliceInPort := getFreePortInRange(t, 26100, 26249)
	aliceOutPort := getFreePortInRange(t, 26250, 26399)
	bobInPort := getFreePortInRange(t, 26400, 26549)
	bobOutPort := getFreePortInRange(t, 26550, 26699)

	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		isFuse, relayURL, aliceInPort, aliceOutPort,
		aliceMount, aliceSave, true, true, "::1")
	require.NoError(err)

	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		isFuse, relayURL, bobInPort, bobOutPort,
		bobMount, bobSave, true, true, "::1")
	require.NoError(err)

	// Exchange fingerprints (simulates out-of-band exchange via Signal etc.)
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

	// Both call Connect() simultaneously. No WaitForCondition is needed:
	// the retry loop in JoinRoom handles the timing.
	aliceReady := testkit.Go(func() error { return kdAlice.Connect() })
	bobReady := testkit.Go(func() error { return kdBob.Connect() })

	testkit.Run(t, func() error {
		return fp.Steps(
			func() error { return testkit.WithinCtx(ctx, "Alice Connect", aliceReady) },
			func() error { return testkit.WithinCtx(ctx, "Bob Connect", bobReady) },
		)
	})

	// The Run() goroutine sets the running flag after it receives the Start
	// signal. Wait for both flags: a Stop() before the flag is set is a no-op,
	// and the queued Start then resurrects the session the test believes dead.
	WaitForCondition(t, 5*time.Second, 10*time.Millisecond, func() bool {
		return kdAlice.IsRunning() && kdBob.IsRunning()
	}, "waiting for both peers to be running after connect")

	tp := &TestPair{
		Alice:         kdAlice,
		Bob:           kdBob,
		Relay:         relay,
		Cancel:        cancel,
		AliceSaveDir:  aliceSave,
		BobSaveDir:    bobSave,
		AliceMountDir: aliceMount,
		BobMountDir:   bobMount,
		runWg:         &runWg,
	}

	t.Cleanup(tp.Teardown)
	return tp
}

// TestConnect_DeterministicRole verifies that Connect() establishes a
// connection using the fingerprint-based tiebreaker. The peer with the
// lower fingerprint becomes the creator.
func TestConnect_DeterministicRole(t *testing.T) {
	tp := SetupPeerPairViaConnect(t, false)
	require := require.New(t)

	// Both should be connected (wait briefly for Run() to process Start signal).
	WaitForCondition(t, 5*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.IsRunning() && tp.Bob.IsRunning()
	}, "both peers should be running")

	// Connection mode should be set.
	require.NotEmpty(tp.Alice.ConnectionMode, "Alice connection mode should be set")
	require.NotEmpty(tp.Bob.ConnectionMode, "Bob connection mode should be set")
}

// TestConnect_FileTransferWorks verifies that file transfer works through
// a Connect()-established connection (end-to-end via the new path).
func TestConnect_FileTransferWorks(t *testing.T) {
	tp := SetupPeerPairViaConnect(t, false)
	require := require.New(t)

	content := []byte("hello from Connect() path")
	filePath := filepath.Join(tp.AliceSaveDir, "connect-test.txt")
	require.NoError(os.WriteFile(filePath, content, 0644))

	err := tp.Alice.AddFile(filePath)
	require.NoError(err)

	WaitForRemoteFile(t, tp.Bob.SyncTracker, "connect-test.txt", 5*time.Second)

	pullDest := filepath.Join(tp.BobSaveDir, "connect-test.txt")
	err = tp.Bob.PullFile("connect-test.txt", pullDest)
	require.NoError(err)

	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content, pulled)
}

// TestConnect_IdenticalFingerprints verifies that Connect() returns
// ErrIdenticalFingerprints when both peers have the same fingerprint.
func TestConnect_IdenticalFingerprints(t *testing.T) {
	require := require.New(t)

	logger := testkit.StdoutLogger(slog.LevelError)
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26700, 26701, "", t.TempDir(), false, false, "::1")
	require.NoError(err)

	// Set peer fingerprint to own fingerprint.
	ownFP, err := kd.ExportFingerprint()
	require.NoError(err)
	require.NoError(kd.AddPeerFingerprint(ownFP))

	err = kd.Connect()
	require.ErrorIs(err, common.ErrIdenticalFingerprints)
}

// TestConnect_EmptyPeerFingerprint verifies that Connect() returns
// ErrEmptyFingerprint when no peer fingerprint has been set.
func TestConnect_EmptyPeerFingerprint(t *testing.T) {
	require := require.New(t)

	logger := testkit.StdoutLogger(slog.LevelError)
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := common.NewKeibiDropWithIP(ctx, logger, false, relayURL, 26702, 26703, "", t.TempDir(), false, false, "::1")
	require.NoError(err)

	err = kd.Connect()
	require.ErrorIs(err, common.ErrEmptyFingerprint)
}

// TestConnect_RetryOn404 verifies that the joiner retries when the creator
// hasn't registered yet (404 from relay).
func TestConnect_RetryOn404(t *testing.T) {
	require := require.New(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	relay := NewMockRelay()
	relayURL, err := url.Parse(relay.URL())
	require.NoError(err)

	logger := testkit.StdoutLogger(slog.LevelWarn)

	aliceInPort := getFreePortInRange(t, 26100, 26249)
	aliceOutPort := getFreePortInRange(t, 26250, 26399)
	bobInPort := getFreePortInRange(t, 26400, 26549)
	bobOutPort := getFreePortInRange(t, 26550, 26699)

	kdAlice, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "alice"),
		false, relayURL, aliceInPort, aliceOutPort,
		"", t.TempDir(), true, true, "::1")
	require.NoError(err)

	kdBob, err := common.NewKeibiDropWithIP(ctx, logger.With("peer", "bob"),
		false, relayURL, bobInPort, bobOutPort,
		"", t.TempDir(), true, true, "::1")
	require.NoError(err)

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

	// Determine which peer is the joiner (higher fingerprint).
	var creator, joiner *common.KeibiDrop
	if aliceFp < bobFp {
		creator = kdAlice
		joiner = kdBob
	} else {
		creator = kdBob
		joiner = kdAlice
	}

	// Start joiner FIRST — relay has nothing yet, so it will get 404s and retry.
	require.Equal(0, relay.EntryCount(), "relay should be empty before creator registers")

	joinerReady := testkit.Go(func() error { return joiner.Connect() })

	// Wait a moment to let joiner hit some 404 retries, then start creator.
	time.Sleep(2 * time.Second)
	require.Equal(0, relay.EntryCount(), "relay should still be empty")

	creatorReady := testkit.Go(func() error { return creator.Connect() })

	// Both should eventually succeed.
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error { return testkit.WithinCtx(ctx, "creator Connect", creatorReady) },
			func() error { return testkit.WithinCtx(ctx, "joiner Connect", joinerReady) },
		)
	})

	WaitForCondition(t, 5*time.Second, 100*time.Millisecond, func() bool {
		return creator.IsRunning() && joiner.IsRunning()
	}, "both peers should be running after retry")

	t.Cleanup(func() {
		kdAlice.StopConnectionResilience()
		kdBob.StopConnectionResilience()
		kdAlice.Shutdown()
		kdBob.Shutdown()
		// A timeout is not a failure here. Teardown proceeds either way.
		_ = testkit.Within(5*time.Second, "peer Run goroutines", func() error { runWg.Wait(); return nil })
		relay.Close()
	})
}
