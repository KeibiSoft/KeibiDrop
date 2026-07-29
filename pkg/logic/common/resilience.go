// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
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

// newConfiguredHealthMonitor builds a fully-wired health monitor. Both initial setup and the
// post-reconnect rebuild go through here so their config can't drift. RekeyEnabled is the
// constructor default; onRekeyNeeded owns the rotation decision.
func (kd *KeibiDrop) newConfiguredHealthMonitor(sess *session.Session, client bindings.KeibiServiceClient, logger *slog.Logger) *session.HealthMonitor {
	hm := session.NewHealthMonitor(sess, client, kd.logger)
	hm.OnDisconnect = kd.onDisconnect
	hm.OnRekeyNeeded = kd.onRekeyNeeded
	// Near-wrap rescue watches the QUIC lane too: it ratchets independently of the TCP pair,
	// and the rescue re-handshake re-derives the QUIC keys.
	hm.ExtraEpoch = kd.QUICWriterEpoch
	hm.OnHealthChange = func(old, cur session.ConnectionHealth) {
		logger.Info("Connection health changed", "from", old, "to", cur)
	}
	return hm
}

func (kd *KeibiDrop) InitConnectionResilience() error {
	if kd.session == nil || kd.KDClient == nil {
		return fmt.Errorf("session or gRPC client not initialized")
	}

	logger := kd.logger.With("method", "init-connection-resilience")

	// Debug-only: KD_REKEY_BYTES/KD_REKEY_MSGS lower the rotation thresholds so tests can force
	// a rekey without moving a gigabyte. Clamped to fire sooner only, never disable. The env
	// read sits behind the debug build tag (rekey_override_debug.go); plain builds get zeros.
	if b, m := rekeyThresholdOverrideFromEnv(); b != 0 || m != 0 {
		ab, am := session.ApplyRekeyThresholdOverride(b, m)
		logger.Warn("DEBUG rekey threshold override active (testing only)", "bytes", ab, "msgs", am)
	}

	// RekeyEnabled defaults on in NewHealthMonitor so reconnect rebuilds can't drop it;
	// onRekeyNeeded decides whether a proposed rotation actually fires.
	kd.HealthMonitor = kd.newConfiguredHealthMonitor(kd.session, kd.KDClient, logger)

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
			kd.mu.Lock()
			sess := kd.session
			kd.mu.Unlock()
			if sess == nil {
				return "", 0, fmt.Errorf("session nil during relay lookup")
			}
			return kd.PeerIPv6IP, sess.PeerPort, nil
		}
	}
	kd.ReconnectManager.AcceptConn = func(timeout time.Duration) (net.Conn, error) {
		// If a prior bridge fallback closed and nil'd the listener before reopening, Accept()
		// on a nil interface panics.
		if kd.listener == nil {
			return nil, fmt.Errorf("accept-conn: inbound listener not open")
		}
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

	// Skip relay keepalive in local mode (no relay to keep alive).
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

// rekeyCooldown bounds how often a proactive rekey may fire, so a stuck reconnect
// cannot thrash the connection.
const rekeyCooldown = 60 * time.Second

// maxDisconnectDeferrals caps consecutive health-failure disconnects the active-transfer guard
// may defer. A transfer blocked on a dead conn never finishes, so an unbounded deferral
// deadlocks recovery; at the cap the reconnect proceeds and resumption recovers the transfer.
const maxDisconnectDeferrals = 3

// onRekeyNeeded rotates the session keys for forward secrecy when idle and past the
// data/message threshold, via a full re-handshake (drop the sockets, let the reconnect path
// re-establish with fresh keys). Only the reconnect-initiator drives it; the peer follows via
// disconnect recovery. Returns true only when it actually initiates a rotation.
func (kd *KeibiDrop) onRekeyNeeded() bool {
	// Snapshot session + reconnect-manager under kd.mu: teardown nils them concurrently.
	kd.mu.Lock()
	sess := kd.session
	rm := kd.ReconnectManager
	kd.mu.Unlock()
	if rm == nil || sess == nil || !rm.IsReconnectInitiator() {
		return false // a single initiator drives the rekey; the responder reconnects in response
	}
	if rm.State() == session.ReconnectStateReconnecting {
		return false // a reconnect is already in flight; do not re-enter it
	}
	// The in-band ratchet rotates zero-pause, so re-handshake rekey is the fallback for old
	// peers only; defer to it, except near the epoch-wrap guard where the ratchet stops
	// advancing and even a key-update pair must re-handshake for a fresh epoch-0 key.
	if sess.UseKeyUpdate() && !sess.NearEpochWrap() {
		return false
	}

	// Never rekey while a transfer is in flight: a trickle under the idle gate would be cut
	// mid-stream if we dropped the sockets.
	if kd.hasActiveTransfers() {
		return false
	}

	// Never rekey while a REMOVE/RENAME notify is queued or in flight: the worker has no retry
	// queue and only ADD_FILE is replayed, so a drop would lose the delete/rename.
	// pendingNotifies counts from dequeue, so also check the channel for a just-enqueued one.
	if len(kd.notifyCh) > 0 || kd.pendingNotifies.Load() > 0 {
		return false
	}

	sess.RekeyMu.Lock()
	if !sess.LastRekeyAt.IsZero() && time.Since(sess.LastRekeyAt) < rekeyCooldown {
		sess.RekeyMu.Unlock()
		return false
	}
	sess.LastRekeyAt = time.Now()
	sess.CurrentEpoch++
	epoch := sess.CurrentEpoch
	sess.RekeyMu.Unlock()

	kd.logger.With("event", "rekey").Info("proactive forward-secrecy rekey via re-handshake", "epoch", epoch)

	// Drop the current sockets so both peers re-handshake with fresh keys, then enter the
	// initiator reconnect role. The responder follows when its health monitor sees the drop.
	if in, out := sess.BothConns(); in != nil || out != nil {
		if in != nil {
			_ = in.Close()
		}
		if out != nil {
			_ = out.Close()
		}
	}
	rm.OnDisconnect()
	return true
}

// onDisconnect is called by health monitor when connection is lost.
func (kd *KeibiDrop) onDisconnect() {
	logger := kd.logger.With("event", "disconnect")

	// Don't tear down while transfers are active: heartbeat failures during large transfers
	// are expected (file data starves heartbeat RPCs). Bounded by maxDisconnectDeferrals: a
	// transfer stuck on a dead conn holds the active count forever, so an unbounded deferral
	// starves the very reconnect it waits on. After the cap we reconnect anyway.
	if kd.hasActiveTransfers() {
		n := kd.disconnectDeferrals.Add(1)
		if n < maxDisconnectDeferrals {
			logger.Warn("Heartbeat failed but active transfers in progress, deferring disconnect",
				"deferrals", n, "max", maxDisconnectDeferrals)
			return
		}
		logger.Warn("Active transfers still blocked after repeated failed heartbeats; reconnecting anyway",
			"deferrals", n)
	}
	kd.disconnectDeferrals.Store(0)

	logger.Warn("Connection lost, initiating reconnection")

	// Push event to UI so it can react immediately.
	if kd.OnEvent != nil {
		kd.OnEvent("peer_disconnected:health_timeout")
	}

	// Snapshot the resilience handles under kd.mu (teardown nils them); call their methods
	// outside the lock to keep the rk.mu -> kd.mu order intact.
	kd.mu.Lock()
	rk := kd.RelayKeepalive
	rm := kd.ReconnectManager
	kd.mu.Unlock()

	// Pause relay keepalive during reconnection
	if rk != nil {
		rk.Pause()
	}

	// Trigger reconnection
	if rm != nil {
		rm.OnDisconnect()
	}
}

// onReconnected rebuilds the gRPC client and server on the fresh P2P sockets so subsequent
// RPCs use the new connection.
func (kd *KeibiDrop) onReconnected() {
	logger := kd.logger.With("event", "reconnected")
	// Bail if Run's teardown has begun: don't rebuild a stack it is concurrently
	// niling. Teardown sets tearingDown before joining this goroutine.
	if kd.tearingDown.Load() {
		logger.Info("Reconnect ignored: shutting down")
		return
	}
	logger.Info("Connection restored")

	// Snapshot the resilience handles / session under kd.mu; teardown rewrites them.
	kd.mu.Lock()
	rm := kd.ReconnectManager
	sess := kd.session
	rk := kd.RelayKeepalive
	hm := kd.HealthMonitor
	kd.mu.Unlock()

	// Update cached peer info.
	if rm != nil && sess != nil {
		rm.CachedPeerIP = kd.PeerIPv6IP
		rm.CachedPeerPort = sess.PeerPort
	}

	// Resume relay keepalive.
	if rk != nil {
		rk.Resume()
		_ = rk.ForceRefresh()
	}

	// Stop the old health monitor before tearing down gRPC so it doesn't heartbeat the dead
	// client and trigger a spurious second reconnection.
	if hm != nil {
		hm.Stop()
	}

	// Tear down stale gRPC infrastructure. Swap the handles out under kd.mu (pairs with
	// teardown's locked swap) and Stop()/Close() the locals outside the lock.
	kd.mu.Lock()
	oldServer := kd.grpcServer
	oldConn := kd.grpcClientConn
	kd.grpcServer = nil
	kd.grpcClientConn = nil
	kd.KDClient = nil
	kd.mu.Unlock()
	if oldServer != nil {
		oldServer.Stop()
	}
	if oldConn != nil {
		oldConn.Close()
	}

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

	// Teardown may have begun during the 15s connect: don't resurrect a health monitor on a
	// session Run is tearing down (leaks a goroutine on a dead session).
	if kd.tearingDown.Load() {
		logger.Info("Reconnect finished during shutdown; skipping monitor restart")
		return
	}

	// The re-handshake derived fresh QUIC keys, so the old QUIC control pair is
	// cryptographically dead. Restart it (idempotent: stops the old one).
	kd.StartQUICControlChannel()

	// Restart health monitoring with the fresh client. Snapshot kd.session under kd.mu
	// (teardown writes it) and publish the new monitor under the lock.
	kd.mu.Lock()
	sess2 := kd.session
	client := kd.KDClient
	kd.mu.Unlock()
	if client != nil && sess2 != nil {
		hmNew := kd.newConfiguredHealthMonitor(sess2, client, logger)
		kd.mu.Lock()
		kd.HealthMonitor = hmNew
		kd.mu.Unlock()
		hmNew.Start()
	}

	// Auto-resume partial downloads for this peer (receiver-initiated).
	go kd.resumePartialDownloads(logger)

	// Re-fold on the fresh session key so forward secrecy is restored after the reconnect.
	kd.maybeStartEagerFold()
}

// notifyRestoredFiles sends ADD_FILE for each restored LocalFile so the peer
// can see them. Only called after same-peer reconnection with confirmed files.
func (kd *KeibiDrop) notifyRestoredFiles(logger *slog.Logger) {
	// Snapshot under kd.mu: spawned as a goroutine, so teardown can nil kd.KDClient.
	kd.mu.Lock()
	client := kd.KDClient
	kd.mu.Unlock()
	if client == nil {
		return
	}
	kd.SyncTracker.LocalFilesMu.RLock()
	files := make(map[string]*synctracker.File, len(kd.SyncTracker.LocalFiles))
	for k, v := range kd.SyncTracker.LocalFiles {
		files[k] = v
	}
	kd.SyncTracker.LocalFilesMu.RUnlock()

	for _, file := range files {
		if kd.ctx.Err() != nil {
			return
		}
		info, err := os.Stat(filepath.Clean(file.RealPathOfFile))
		if err != nil {
			continue
		}
		_, _ = client.Notify(kd.ctx, &bindings.NotifyRequest{
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
	// Snapshot kd.session under kd.mu: this detached goroutine runs while teardown can nil
	// kd.session concurrently.
	kd.mu.Lock()
	sess := kd.session
	kd.mu.Unlock()
	if kd.dlRegistry == nil || sess == nil || kd.SyncTracker == nil {
		return
	}
	tag := kd.dlRegistry.peerTag(sess.ExpectedPeerFingerprint, kd.registryKey)
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
			// Peer doesn't have it anymore, clean up.
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

// WriterEpoch reports the current in-band ratchet generation (max writer epoch across both live
// directions), or 0 before a monitor exists. Pull-based and race-free (reads the monitor's
// captured conns).
func (kd *KeibiDrop) WriterEpoch() uint16 {
	hm := kd.HealthMonitor
	if hm == nil {
		return 0
	}
	return hm.MaxWriterEpoch()
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
