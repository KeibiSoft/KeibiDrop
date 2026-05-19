// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
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
			if newIP, err := GetGlobalIPv6(); err == nil && newIP != "" && newIP != kd.LocalIPv6IP {
				logger.Info("IP changed before reconnect, updating", "old", kd.LocalIPv6IP, "new", newIP)
				kd.LocalIPv6IP = newIP
			}
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
		kd.ReconnectManager.DialBridge = func(direction string) (net.Conn, error) {
			return kd.dialBridgeDir(direction, kd.logger)
		}
	}
	kd.ReconnectManager.OnReconnected = kd.onReconnected

	kd.wireReconnectEvents()

	// Start all components
	kd.HealthMonitor.Start()

	// Skip relay keepalive in local mode — no relay to keep alive.
	if !kd.IsLocalMode {
		kd.RelayKeepalive = NewRelayKeepalive(kd, kd.logger)
		kd.RelayKeepalive.Start()
	}

	// Restore shared files if reconnecting to the same peer.
	if kd.lastSharedFiles != nil && kd.lastSharedPeerFP != "" &&
		kd.session.ExpectedPeerFingerprint == kd.lastSharedPeerFP {
		kd.SyncTracker.LocalFilesMu.Lock()
		for k, v := range kd.lastSharedFiles {
			kd.SyncTracker.LocalFiles[k] = v
		}
		kd.SyncTracker.LocalFilesMu.Unlock()
		logger.Info("Restored shared files for same peer (memory)", "count", len(kd.lastSharedFiles))
		go kd.notifyRestoredFiles(logger)
	} else if kd.sharedStore != nil && kd.dlRegistry != nil {
		tag := kd.dlRegistry.peerTag(kd.session.ExpectedPeerFingerprint, kd.registryKey)
		entries := kd.sharedStore.Load()
		var restored int
		kd.SyncTracker.LocalFilesMu.Lock()
		for _, e := range entries {
			if e.PeerTag != tag {
				continue
			}
			cleanPath := filepath.Clean(e.Path)
			info, err := os.Stat(cleanPath)
			if err != nil {
				continue
			}
			name := filepath.Base(cleanPath)
			kd.SyncTracker.LocalFiles[name] = &synctracker.File{
				Name:           name,
				RelativePath:   name,
				RealPathOfFile: cleanPath,
				Size:           uint64(info.Size()),
				LastEditTime:   uint64(info.ModTime().UnixNano()),
			}
			restored++
		}
		kd.SyncTracker.LocalFilesMu.Unlock()
		if restored > 0 {
			logger.Info("Restored shared files for same peer (disk)", "count", restored)
			go kd.notifyRestoredFiles(logger)
		}
		if restored == 0 && len(entries) > 0 {
			kd.sharedStore.Clear()
		}
	}
	kd.lastSharedFiles = nil
	kd.lastSharedPeerFP = ""

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

	// Auto-resume partial downloads for this peer (receiver-initiated).
	go kd.resumePartialDownloads(logger)
}

// notifyRestoredFiles sends ADD_FILE for each restored LocalFile so the peer
// can see them. Only called after same-peer reconnection with confirmed files.
func (kd *KeibiDrop) notifyRestoredFiles(logger *slog.Logger) {
	if kd.KDClient == nil {
		return
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	files := make(map[string]*synctracker.File, len(kd.SyncTracker.LocalFiles))
	for k, v := range kd.SyncTracker.LocalFiles {
		files[k] = v
	}
	kd.SyncTracker.LocalFilesMu.RUnlock()

	for _, file := range files {
		info, err := os.Stat(filepath.Clean(file.RealPathOfFile))
		if err != nil {
			continue
		}
		_, _ = kd.KDClient.Notify(context.Background(), &bindings.NotifyRequest{
			Type: bindings.NotifyType(types.AddFile),
			Path: file.RelativePath,
			Attr: &bindings.Attr{
				Mode:             uint32(info.Mode().Perm()) | 0100000,
				Size:             info.Size(),
				ModificationTime: uint64(info.ModTime().UnixNano()),
				ChangeTime:       uint64(info.ModTime().UnixNano()),
				BirthTime:        uint64(info.ModTime().UnixNano()),
			},
		})
		logger.Info("Re-notified peer about restored file", "path", file.RelativePath)
	}
}

// resumePartialDownloads checks the registry for bitmaps belonging to the
// current peer and auto-resumes them. Called after successful reconnection.
func (kd *KeibiDrop) resumePartialDownloads(logger *slog.Logger) {
	if kd.dlRegistry == nil || kd.session == nil || kd.SyncTracker == nil {
		return
	}
	tag := kd.dlRegistry.peerTag(kd.session.ExpectedPeerFingerprint, kd.registryKey)
	paths := kd.dlRegistry.ForPeer(tag)
	if len(paths) == 0 {
		return
	}

	// Emit event so UI can show toast.
	if kd.OnEvent != nil {
		kd.OnEvent(fmt.Sprintf("resuming_downloads:%d", len(paths)))
	}

	for _, bitmapPath := range paths {
		localPath := strings.TrimSuffix(bitmapPath, ".kdbitmap")
		remoteName := strings.TrimPrefix(localPath, kd.ToSave+"/")
		if remoteName == localPath {
			remoteName = strings.TrimPrefix(localPath, kd.ToSave)
		}
		remoteName = strings.TrimPrefix(remoteName, "/")
		if remoteName == "" {
			continue
		}

		// Load bitmap to get file size for the synthetic entry.
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			kd.dlRegistry.Unregister(bitmapPath)
			os.Remove(bitmapPath)
			continue
		}

		// Add synthetic RemoteFiles entry so PullFile can find it.
		kd.SyncTracker.RemoteFilesMu.Lock()
		if _, exists := kd.SyncTracker.RemoteFiles[remoteName]; !exists {
			kd.SyncTracker.RemoteFiles[remoteName] = &synctracker.File{
				RelativePath: remoteName,
				Size:         uint64(info.Size()),
			}
		}
		kd.SyncTracker.RemoteFilesMu.Unlock()

		logger.Info("Auto-resuming download", "remoteName", remoteName)
		err := kd.PullFile(remoteName, localPath)
		if err != nil {
			logger.Warn("Resume failed, peer may have deleted file", "remoteName", remoteName, "err", err)
			// Peer doesn't have it anymore — clean up.
			if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "NotFound") {
				kd.dlRegistry.Unregister(bitmapPath)
				os.Remove(bitmapPath)
				os.Remove(localPath)
			}
		}
	}
}

func (kd *KeibiDrop) wireReconnectEvents() {
	if kd.ReconnectManager == nil {
		return
	}

	// wrapCb chains an event emission after an existing callback.
	wrapCb := func(orig func(), event string) func() {
		return func() {
			if orig != nil {
				orig()
			}
			if kd.OnEvent != nil {
				kd.OnEvent(event)
			}
		}
	}

	kd.ReconnectManager.OnReconnecting = wrapCb(kd.ReconnectManager.OnReconnecting, "reconnecting:")
	kd.ReconnectManager.OnReconnected = wrapCb(kd.ReconnectManager.OnReconnected, "reconnected:")
	kd.ReconnectManager.OnGaveUp = wrapCb(kd.ReconnectManager.OnGaveUp, "gave_up:")
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
