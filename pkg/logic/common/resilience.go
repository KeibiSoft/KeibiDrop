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
	if !kd.IsLocalMode {
		kd.ReconnectManager.RelayRefresh = func() error {
			return kd.registerRoomToRelay()
		}
		kd.ReconnectManager.RelayLookup = func(fingerprint string) (string, int, error) {
			if err := kd.getRoomFromRelay(fingerprint); err != nil {
				return "", 0, err
			}
			return kd.PeerIPv6IP, kd.session.PeerPort, nil
		}
	}
	kd.ReconnectManager.AcceptConn = func(timeout time.Duration) (net.Conn, error) {
		if tcpL, ok := kd.listener.(*net.TCPListener); ok {
			_ = tcpL.SetDeadline(time.Now().Add(timeout))
			return tcpL.Accept()
		}
		return kd.listener.Accept()
	}
	if kd.BridgeAddr != "" {
		kd.ReconnectManager.BridgeAddr = kd.BridgeAddr
		kd.ReconnectManager.DialBridge = func() (net.Conn, error) {
			return kd.dialBridge(kd.logger)
		}
	}
	kd.ReconnectManager.OnReconnected = kd.onReconnected

	// Start all components
	kd.HealthMonitor.Start()

	// Skip relay keepalive in local mode — no relay to keep alive.
	if !kd.IsLocalMode {
		kd.RelayKeepalive = NewRelayKeepalive(kd, kd.logger)
		kd.RelayKeepalive.Start()
	}

	logger.Info("Connection resilience initialized")
	return nil
}

// StopConnectionResilience stops all resilience components.
func (kd *KeibiDrop) StopConnectionResilience() {
	logger := kd.logger.With("method", "stop-connection-resilience")
	if kd.HealthMonitor != nil {
		logger.Info("stopping health monitor")
		kd.HealthMonitor.Stop()
		logger.Info("health monitor stopped")
	}
	if kd.ReconnectManager != nil {
		logger.Info("stopping reconnect manager")
		kd.ReconnectManager.Stop()
		logger.Info("reconnect manager stopped")
	}
	if kd.RelayKeepalive != nil {
		logger.Info("stopping relay keepalive")
		kd.RelayKeepalive.Stop()
		logger.Info("relay keepalive stopped")
	}
	logger.Info("all resilience components stopped")
}

// hasActiveTransfers returns true if any downloads are in progress.
// Used to avoid tearing down the connection during active file transfers.
func (kd *KeibiDrop) hasActiveTransfers() bool {
	kd.activeDownloadsMu.Lock()
	defer kd.activeDownloadsMu.Unlock()
	return len(kd.activeDownloads) > 0
}

// onDisconnect is called by health monitor when connection is lost.
func (kd *KeibiDrop) onDisconnect() {
	logger := kd.logger.With("event", "disconnect")

	// Don't tear down the connection while transfers are active.
	// Heartbeat failures during large transfers are expected (the gRPC
	// connection is saturated with file data, starving heartbeat RPCs).
	if kd.hasActiveTransfers() {
		logger.Warn("Heartbeat failed but active transfers in progress, deferring disconnect")
		return
	}

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
// It rebuilds the gRPC client and server on the fresh P2P sockets so that
// all subsequent RPCs (file ops, health checks) use the new connection.
func (kd *KeibiDrop) onReconnected() {
	logger := kd.logger.With("event", "reconnected")
	logger.Info("Connection restored")

	// Update cached peer info.
	if kd.ReconnectManager != nil {
		kd.ReconnectManager.CachedPeerIP = kd.PeerIPv6IP
		kd.ReconnectManager.CachedPeerPort = kd.session.PeerPort
	}

	// Resume relay keepalive.
	if kd.RelayKeepalive != nil {
		kd.RelayKeepalive.Resume()
		_ = kd.RelayKeepalive.ForceRefresh()
	}

	// Tear down stale gRPC infrastructure.
	if kd.grpcServer != nil {
		kd.grpcServer.Stop()
		kd.grpcServer = nil
	}
	if kd.grpcClientConn != nil {
		kd.grpcClientConn.Close()
		kd.grpcClientConn = nil
	}
	kd.KDClient = nil

	// Rebuild gRPC server on the new inbound socket.
	go func() {
		if err := kd.startGRPCServer(); err != nil {
			logger.Error("Failed to restart gRPC server after reconnect", "err", err)
		}
	}()

	// Rebuild gRPC client on the new outbound socket.
	if err := kd.connectGRPCClientWithRetry(15 * time.Second); err != nil {
		logger.Error("Failed to reconnect gRPC client", "err", err)
		return
	}

	// Restart health monitoring with the fresh client.
	if kd.HealthMonitor != nil {
		kd.HealthMonitor.Stop()
	}
	if kd.KDClient != nil {
		kd.HealthMonitor = session.NewHealthMonitor(kd.session, kd.KDClient, kd.logger)
		kd.HealthMonitor.OnDisconnect = kd.onDisconnect
		kd.HealthMonitor.Start()
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
