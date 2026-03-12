// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"fmt"
	"net"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// InitConnectionResilience sets up health monitoring, reconnection, and relay keepalive.
// Call this after the session is connected and gRPC client is ready.
func (kd *KeibiDrop) InitConnectionResilience() error {
	if kd.session == nil || kd.KDClient == nil {
		return fmt.Errorf("session or gRPC client not initialized")
	}

	logger := kd.logger.With("method", "init-connection-resilience")

	// Initialize health monitor
	kd.HealthMonitor = session.NewHealthMonitor(kd.session, kd.KDClient, kd.logger)
	kd.HealthMonitor.OnDisconnect = kd.onDisconnect
	kd.HealthMonitor.OnHealthChange = func(old, new session.ConnectionHealth) {
		logger.Info("Connection health changed", "from", old, "to", new)
	}

	// Initialize reconnection manager
	kd.ReconnectManager = session.NewReconnectManager(kd.session, kd.logger)
	kd.ReconnectManager.CachedPeerIP = kd.PeerIPv6IP
	kd.ReconnectManager.CachedPeerPort = kd.session.PeerPort
	kd.ReconnectManager.RelayRefresh = func() error {
		return kd.registerRoomToRelay()
	}
	kd.ReconnectManager.RelayLookup = func(fingerprint string) (string, int, error) {
		if err := kd.getRoomFromRelay(fingerprint); err != nil {
			return "", 0, err
		}
		return kd.PeerIPv6IP, kd.session.PeerPort, nil
	}
	kd.ReconnectManager.AcceptConn = func(timeout time.Duration) (net.Conn, error) {
		if tcpL, ok := kd.listener.(*net.TCPListener); ok {
			tcpL.SetDeadline(time.Now().Add(timeout))
			return tcpL.Accept()
		}
		return kd.listener.Accept()
	}
	kd.ReconnectManager.OnReconnected = kd.onReconnected

	// Initialize relay keepalive
	kd.RelayKeepalive = NewRelayKeepalive(kd, kd.logger)

	// Start all components
	kd.HealthMonitor.Start()
	kd.RelayKeepalive.Start()

	logger.Info("Connection resilience initialized")
	return nil
}

// StopConnectionResilience stops all resilience components.
func (kd *KeibiDrop) StopConnectionResilience() {
	if kd.HealthMonitor != nil {
		kd.HealthMonitor.Stop()
	}
	if kd.ReconnectManager != nil {
		kd.ReconnectManager.Stop()
	}
	if kd.RelayKeepalive != nil {
		kd.RelayKeepalive.Stop()
	}
}

// onDisconnect is called by health monitor when connection is lost.
func (kd *KeibiDrop) onDisconnect() {
	logger := kd.logger.With("event", "disconnect")
	logger.Warn("Connection lost, initiating reconnection")

	// Push event to UI so it can react immediately.
	if kd.OnEvent != nil {
		kd.OnEvent("peer_disconnected:health_timeout")
	}

	// Pause relay keepalive during reconnection
	if kd.RelayKeepalive != nil {
		kd.RelayKeepalive.Pause()
	}

	// Trigger reconnection
	if kd.ReconnectManager != nil {
		kd.ReconnectManager.OnDisconnect()
	}
}

// onReconnected is called when reconnection succeeds.
func (kd *KeibiDrop) onReconnected() {
	logger := kd.logger.With("event", "reconnected")
	logger.Info("Connection restored")

	// Update cached peer info
	if kd.ReconnectManager != nil {
		kd.ReconnectManager.CachedPeerIP = kd.PeerIPv6IP
		kd.ReconnectManager.CachedPeerPort = kd.session.PeerPort
	}

	// Resume relay keepalive
	if kd.RelayKeepalive != nil {
		kd.RelayKeepalive.Resume()
		// Force refresh with new connection info
		kd.RelayKeepalive.ForceRefresh()
	}

	// Restart health monitoring with new connection
	if kd.HealthMonitor != nil {
		kd.HealthMonitor.Stop()
		// Reinitialize with potentially new gRPC client
		if kd.KDClient != nil {
			kd.HealthMonitor = session.NewHealthMonitor(kd.session, kd.KDClient, kd.logger)
			kd.HealthMonitor.OnDisconnect = kd.onDisconnect
			kd.HealthMonitor.Start()
		}
	}
}

// ConnectionStatus returns the current connection health status.
func (kd *KeibiDrop) ConnectionStatus() string {
	if kd.HealthMonitor == nil {
		return "unknown"
	}
	return kd.HealthMonitor.Health().String()
}

// ReconnectionState returns the current reconnection state.
func (kd *KeibiDrop) ReconnectionState() string {
	if kd.ReconnectManager == nil {
		return "unknown"
	}
	return kd.ReconnectManager.State().String()
}

// ReconnectionAttempts returns the number of reconnection attempts.
func (kd *KeibiDrop) ReconnectionAttempts() int {
	if kd.ReconnectManager == nil {
		return 0
	}
	return kd.ReconnectManager.Attempts()
}
