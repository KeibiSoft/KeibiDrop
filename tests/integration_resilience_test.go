// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// TestResilience_InitAfterConnect verifies that connection resilience
// (health monitor, reconnect manager, relay keepalive) is automatically
// initialized after peers connect via CreateRoom/JoinRoom.
func TestResilience_InitAfterConnect(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Both peers should have health monitor initialized.
	require.NotNil(tp.Alice.HealthMonitor, "Alice HealthMonitor should be initialized")
	require.NotNil(tp.Bob.HealthMonitor, "Bob HealthMonitor should be initialized")

	// Both should have reconnect manager initialized.
	require.NotNil(tp.Alice.ReconnectManager, "Alice ReconnectManager should be initialized")
	require.NotNil(tp.Bob.ReconnectManager, "Bob ReconnectManager should be initialized")

	// Both should have relay keepalive initialized.
	require.NotNil(tp.Alice.RelayKeepalive, "Alice RelayKeepalive should be initialized")
	require.NotNil(tp.Bob.RelayKeepalive, "Bob RelayKeepalive should be initialized")

	// Connection status API should work.
	require.Equal("healthy", tp.Alice.ConnectionStatus())
	require.Equal("healthy", tp.Bob.ConnectionStatus())

	// Reconnection state should be "connected" (not reconnecting).
	require.Equal("connected", tp.Alice.ReconnectionState())
	require.Equal("connected", tp.Bob.ReconnectionState())

	// Zero reconnection attempts.
	require.Equal(0, tp.Alice.ReconnectionAttempts())
	require.Equal(0, tp.Bob.ReconnectionAttempts())
}

// TestResilience_HeartbeatsWork verifies that the health monitor heartbeat
// RPC actually works between the two peers, producing valid RTT measurements.
func TestResilience_HeartbeatsWork(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Override heartbeat interval to be fast for testing.
	tp.Alice.HealthMonitor.Stop()
	tp.Alice.HealthMonitor.Interval = 500 * time.Millisecond
	tp.Alice.HealthMonitor.Start()

	// Wait for at least 2 heartbeats (1.5 seconds for 500ms interval).
	time.Sleep(1500 * time.Millisecond)

	// Health should still be healthy (heartbeats succeeded).
	require.Equal(session.HealthHealthy, tp.Alice.HealthMonitor.Health(),
		"Alice should be healthy after heartbeats")

	// RTT should be non-zero (heartbeat round-trip measured).
	lastRTT := tp.Alice.HealthMonitor.LastRTT()
	require.Greater(int64(lastRTT), int64(0), "RTT should be positive")

	avgRTT := tp.Alice.HealthMonitor.AvgRTT()
	require.Greater(int64(avgRTT), int64(0), "Avg RTT should be positive")

	// Loopback RTT should be very low (< 100ms).
	require.Less(lastRTT, 100*time.Millisecond, "Loopback RTT should be < 100ms")
}

// TestResilience_FileTransferWithHealthMonitor verifies that file transfers
// still work correctly while health monitoring is active.
func TestResilience_FileTransferWithHealthMonitor(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Verify resilience is running.
	require.NotNil(tp.Alice.HealthMonitor)
	require.NotNil(tp.Bob.HealthMonitor)

	// Do a normal file transfer (Alice → Bob).
	content := []byte("file transfer with health monitor active")
	alicePath := filepath.Join(tp.AliceSaveDir, "health_test.txt")
	require.NoError(os.WriteFile(alicePath, content, 0644))
	require.NoError(tp.Alice.AddFile(alicePath))

	WaitForRemoteFile(t, tp.Bob.SyncTracker, "health_test.txt", 10*time.Second)

	pullDest := filepath.Join(tp.BobSaveDir, "health_test.txt")
	require.NoError(tp.Bob.PullFile("health_test.txt", pullDest))

	pulled, err := os.ReadFile(pullDest)
	require.NoError(err)
	require.Equal(content, pulled)

	// Health should still be healthy after transfer.
	require.Equal("healthy", tp.Alice.ConnectionStatus())
	require.Equal("healthy", tp.Bob.ConnectionStatus())
}

// TestResilience_DisconnectDetection verifies that when one peer's gRPC
// connection breaks, the other peer's health monitor detects the disconnect.
func TestResilience_DisconnectDetection(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	// Reconfigure Alice's health monitor for fast detection.
	tp.Alice.HealthMonitor.Stop()
	tp.Alice.HealthMonitor.Interval = 300 * time.Millisecond
	tp.Alice.HealthMonitor.Timeout = 200 * time.Millisecond
	tp.Alice.HealthMonitor.MaxFailures = 2
	tp.Alice.HealthMonitor.Start()

	// Confirm healthy first.
	time.Sleep(500 * time.Millisecond)
	require.Equal(session.HealthHealthy, tp.Alice.HealthMonitor.Health())

	// Close the underlying TCP connections on Bob's side.
	// This immediately breaks the gRPC transport, causing Alice's heartbeats to fail.
	if tp.Bob.KDSvc != nil && tp.Bob.KDSvc.Session != nil && tp.Bob.KDSvc.Session.Session != nil {
		if tp.Bob.KDSvc.Session.Session.Inbound != nil {
			tp.Bob.KDSvc.Session.Session.Inbound.Close()
		}
		if tp.Bob.KDSvc.Session.Session.Outbound != nil {
			tp.Bob.KDSvc.Session.Session.Outbound.Close()
		}
	}

	// Wait for Alice to detect disconnect (2 failures × 300ms interval + margin).
	WaitForCondition(t, 5*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.HealthMonitor.Health() == session.HealthDisconnected
	}, "waiting for Alice to detect Bob's disconnect")

	// Alice should have detected disconnect.
	require.Equal(session.HealthDisconnected, tp.Alice.HealthMonitor.Health())
	require.Equal("disconnected", tp.Alice.ConnectionStatus())
}

// TestResilience_DeterministicInitiator verifies that the reconnection
// initiator is deterministic (lower fingerprint = initiator).
func TestResilience_DeterministicInitiator(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	require.NotNil(tp.Alice.ReconnectManager)
	require.NotNil(tp.Bob.ReconnectManager)

	// Exactly one peer should be the initiator, the other the responder.
	aliceInit := tp.Alice.ReconnectManager.IsReconnectInitiator()
	bobInit := tp.Bob.ReconnectManager.IsReconnectInitiator()

	require.NotEqual(aliceInit, bobInit,
		"Exactly one peer should be initiator (Alice=%v, Bob=%v)", aliceInit, bobInit)
}

// TestResilience_StopCleanup verifies that StopConnectionResilience
// cleanly shuts down all resilience components without panics.
func TestResilience_StopCleanup(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	require.NotNil(tp.Alice.HealthMonitor)

	// Explicitly stop resilience (should not panic or hang).
	tp.Alice.StopConnectionResilience()
	tp.Bob.StopConnectionResilience()

	// After stopping, health monitor should no longer update.
	// Just verify no panic.
}
