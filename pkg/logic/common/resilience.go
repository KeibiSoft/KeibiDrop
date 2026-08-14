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

// newConfiguredHealthMonitor builds a fully-wired health monitor. Initial setup and
// the post-reconnect rebuild both use it, so their configs cannot drift. RekeyEnabled
// is the constructor default. onRekeyNeeded owns the rotation decision.
func (kd *KeibiDrop) newConfiguredHealthMonitor(sess *session.Session, client bindings.KeibiServiceClient, logger *slog.Logger) *session.HealthMonitor {
	hm := session.NewHealthMonitor(sess, client, kd.logger)
	hm.OnDisconnect = kd.onDisconnect
	hm.OnRekeyNeeded = kd.onRekeyNeeded
	// Near-wrap rescue watches the QUIC lane too. The lane ratchets independently of the
	// TCP pair, and the rescue re-handshake re-derives the QUIC keys.
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

	// Debug-only: KD_REKEY_BYTES/KD_REKEY_MSGS lower the rotation thresholds, so tests
	// force a rekey without moving a gigabyte. The clamp fires sooner only, never
	// disables. The env read sits behind the debug build tag. Plain builds get zeros.
	if b, m := rekeyThresholdOverrideFromEnv(); b != 0 || m != 0 {
		ab, am := session.ApplyRekeyThresholdOverride(b, m)
		logger.Warn("DEBUG rekey threshold override active (testing only)", "bytes", ab, "msgs", am)
	}

	// RekeyEnabled defaults on in NewHealthMonitor, so reconnect rebuilds cannot drop it.
	// onRekeyNeeded decides whether a proposed rotation fires.
	kd.HealthMonitor = kd.newConfiguredHealthMonitor(kd.session, kd.KDClient, logger)

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
		// A prior bridge fallback can close and nil the listener before it reopens.
		// Accept on a nil interface panics.
		if kd.listener == nil {
			return nil, fmt.Errorf("accept-conn: inbound listener not open")
		}
		if tcpL, ok := kd.listener.(*net.TCPListener); ok {
			_ = tcpL.SetDeadline(time.Now().Add(timeout))
			return tcpL.Accept()
		}
		return kd.listener.Accept()
	}
	// Order reconnect transports from the session mode and the reachability hints.
	// A direct retry against a blocked inbound only burns its timeout.
	kd.ReconnectManager.PreferDirect = func() bool {
		if kd.ConnectionMode == "bridge" {
			return false
		}
		return !kd.InboundBlocked() && !kd.PeerInboundBlocked()
	}
	if kd.BridgeAddr != "" {
		kd.ReconnectManager.BridgeAddr = kd.effectiveBridgeAddr()
		kd.ReconnectManager.DialBridge = func(direction string) (net.Conn, error) {
			return kd.dialBridgeDir(direction, kd.logger)
		}
	}
	kd.ReconnectManager.OnReconnected = kd.onReconnected

	kd.wireReconnectEvents()

	kd.HealthMonitor.Start()

	// Local mode has no relay, so skip the relay keepalive.
	if !kd.IsLocalMode {
		kd.RelayKeepalive = NewRelayKeepalive(kd, kd.logger)
		kd.RelayKeepalive.Start()
	}

	// Restore shared files when the same peer reconnects.
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
			rel := e.Rel
			if rel == "" {
				rel = name // entry from an older build: flat announce
			}
			kd.SyncTracker.LocalFiles[rel] = &synctracker.File{
				Name:           name,
				RelativePath:   rel,
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

	// Announce files already in the save folder. Runs after the restore block,
	// so restored files are already tracked and the scan skips them.
	if kd.ScanSharedOnStart {
		ctx := kd.ctx
		go func() {
			n, err := kd.ScanAndShareSaveDir(ctx)
			if err != nil {
				logger.Warn("Save folder scan stopped early", "announced", n, "error", err)
				return
			}
			if n > 0 {
				logger.Info("Announced pre-existing save folder files", "count", n)
			}
		}()
	}

	logger.Info("Connection resilience initialized")
	return nil
}

// StopConnectionResilience stops all resilience components. It snapshots the
// handles under kd.mu, because teardown nils the fields under the same lock and
// external callers run concurrently with it. Stop calls run outside the lock.
func (kd *KeibiDrop) StopConnectionResilience() {
	kd.mu.Lock()
	hm := kd.HealthMonitor
	rm := kd.ReconnectManager
	rk := kd.RelayKeepalive
	kd.mu.Unlock()
	if hm != nil {
		hm.Stop()
	}
	if rm != nil {
		rm.Stop()
	}
	if rk != nil {
		rk.Stop()
	}
}

// hasActiveTransfers reports whether any download is in progress. Callers use it to
// avoid a teardown during active transfers.
func (kd *KeibiDrop) hasActiveTransfers() bool {
	kd.activeDownloadsMu.Lock()
	defer kd.activeDownloadsMu.Unlock()
	return len(kd.activeDownloads) > 0
}

// rekeyCooldown bounds how often a proactive rekey may fire, so a stuck reconnect
// cannot thrash the connection.
const rekeyCooldown = 60 * time.Second

// maxDisconnectDeferrals caps how many disconnects the active-transfer guard defers.
// A transfer blocked on a dead conn never finishes, so an unbounded deferral deadlocks
// recovery. At the cap the reconnect proceeds and resumption recovers the transfer.
const maxDisconnectDeferrals = 3

// onRekeyNeeded rotates the session keys when idle and past the threshold: it drops
// the sockets, and the reconnect path re-handshakes with fresh keys. Only the
// reconnect-initiator drives it. It returns true only when it starts a rotation.
func (kd *KeibiDrop) onRekeyNeeded() bool {
	// Snapshot session and reconnect manager under kd.mu. Teardown nils them concurrently.
	kd.mu.Lock()
	sess := kd.session
	rm := kd.ReconnectManager
	kd.mu.Unlock()
	if rm == nil || sess == nil || !rm.IsReconnectInitiator() {
		return false // One initiator drives the rekey. The responder follows.
	}
	if rm.State() == session.ReconnectStateReconnecting {
		return false // A reconnect is already in flight. Do not re-enter it.
	}
	// The in-band ratchet rotates with zero pause, so re-handshake rekey is only the
	// fallback for old peers. Near the epoch-wrap guard the ratchet stops advancing,
	// and even a key-update pair must re-handshake for a fresh epoch-0 key.
	if sess.UseKeyUpdate() && !sess.NearEpochWrap() {
		return false
	}

	// Never rekey while a transfer is in flight. A socket drop would cut a trickle
	// under the idle gate mid-stream.
	if kd.hasActiveTransfers() {
		return false
	}

	// Never rekey while a REMOVE/RENAME notify is queued or in flight. The worker has no
	// retry queue, and only ADD_FILE replays, so a drop would lose the delete or rename.
	// pendingNotifies counts from dequeue, so also check the channel for a fresh entry.
	// Snapshot the channel under kd.mu: setupFilesystem replaces it on reconnect.
	kd.mu.Lock()
	notifyCh := kd.notifyCh
	kd.mu.Unlock()
	if len(notifyCh) > 0 || kd.pendingNotifies.Load() > 0 {
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

	// Drop the current sockets so both peers re-handshake with fresh keys, then enter
	// the initiator reconnect role. The responder follows when its monitor sees the drop.
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

// onDisconnect runs when the health monitor loses the connection.
func (kd *KeibiDrop) onDisconnect() {
	logger := kd.logger.With("event", "disconnect")

	// Do not tear down during transfers: file data starves heartbeat RPCs, so failures
	// are normal then. A stuck transfer holds the count forever, and an unbounded
	// deferral starves the reconnect it waits on. After the cap, reconnect anyway.
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

	// Push the event to the UI, so it reacts immediately.
	if kd.OnEvent != nil {
		kd.OnEvent("peer_disconnected:health_timeout")
	}

	// Snapshot the resilience handles under kd.mu. Teardown nils them. Call their
	// methods outside the lock to keep the rk.mu -> kd.mu order intact.
	kd.mu.Lock()
	rk := kd.RelayKeepalive
	rm := kd.ReconnectManager
	kd.mu.Unlock()

	if rk != nil {
		rk.Pause()
	}

	if rm != nil {
		rm.OnDisconnect()
	}
}

// onReconnected rebuilds the gRPC client and server on the fresh P2P sockets, so
// later RPCs use the new connection.
func (kd *KeibiDrop) onReconnected() {
	logger := kd.logger.With("event", "reconnected")
	// Stop when Run's teardown has begun: do not rebuild a stack it concurrently nils.
	// Teardown sets tearingDown before it joins this goroutine.
	if kd.tearingDown.Load() {
		logger.Info("Reconnect ignored: shutting down")
		return
	}
	logger.Info("Connection restored")

	// Snapshot the resilience handles and session under kd.mu. Teardown rewrites them.
	kd.mu.Lock()
	rm := kd.ReconnectManager
	sess := kd.session
	rk := kd.RelayKeepalive
	hm := kd.HealthMonitor
	kd.mu.Unlock()

	if rm != nil && sess != nil {
		rm.CachedPeerIP = kd.PeerIPv6IP
		rm.CachedPeerPort = sess.PeerPort
	}

	if rk != nil {
		rk.Resume()
		_ = rk.ForceRefresh()
	}

	// Stop the old health monitor before the gRPC teardown, so it does not heartbeat
	// the dead client and trigger a spurious second reconnection.
	if hm != nil {
		hm.Stop()
	}

	// Tear down stale gRPC state. Swap the handles out under kd.mu, which pairs with
	// teardown's locked swap, and stop or close the locals outside the lock.
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

	// Teardown may have begun during the 15s connect. A monitor on a session Run tears
	// down leaks a goroutine, so skip the restart.
	if kd.tearingDown.Load() {
		logger.Info("Reconnect finished during shutdown; skipping monitor restart")
		return
	}

	// The re-handshake derived fresh QUIC keys, so the old QUIC control pair is dead.
	// Restart it. The restart stops the old one first.
	kd.StartQUICControlChannel()

	// Restart health monitoring with the fresh client. Snapshot kd.session under kd.mu,
	// because teardown writes it, and publish the new monitor under the lock.
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

	// Auto-resume this peer's partial downloads. The receiver initiates.
	go kd.resumePartialDownloads(logger)

	// Re-fold on the fresh session key to restore forward secrecy after the reconnect.
	kd.maybeStartEagerFold()
}

// notifyRestoredFiles sends ADD_FILE for each restored LocalFile, so the peer sees
// them. It runs only after a same-peer reconnect with confirmed files.
func (kd *KeibiDrop) notifyRestoredFiles(logger *slog.Logger) {
	// Snapshot under kd.mu. This runs as a goroutine, and teardown can nil kd.KDClient.
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
		atime, btime := statTimes(info)
		_, _ = client.Notify(kd.ctx, &bindings.NotifyRequest{
			Type: bindings.NotifyType(types.AddFile),
			Path: file.RelativePath,
			Attr: &bindings.Attr{
				Mode:             uint32(info.Mode().Perm()) | 0100000,
				Size:             info.Size(),
				AccessTime:       atime,
				ModificationTime: uint64(info.ModTime().UnixNano()),
				ChangeTime:       uint64(info.ModTime().UnixNano()),
				BirthTime:        btime,
			},
		})
		logger.Info("Re-notified peer about restored file", "path", file.RelativePath)
	}
}

// resumePartialDownloads finds this peer's bitmaps in the registry and resumes them.
// It runs after a successful reconnect.
func (kd *KeibiDrop) resumePartialDownloads(logger *slog.Logger) {
	// Snapshot kd.session under kd.mu. This detached goroutine runs while teardown can
	// nil kd.session concurrently.
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

	// Emit the event, so the UI shows a toast.
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

		// Stat the partial file to get the size for the synthetic entry.
		info, statErr := os.Stat(localPath)
		if statErr != nil {
			kd.dlRegistry.Unregister(bitmapPath)
			os.Remove(bitmapPath)
			continue
		}

		// Add a synthetic RemoteFiles entry, so PullFile can find it.
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
			// The peer no longer has the file. Clean up.
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

// ConnectionStatus returns the current connection health status. It snapshots the
// monitor under kd.mu, because teardown nils the field under the same lock.
func (kd *KeibiDrop) ConnectionStatus() string {
	kd.mu.Lock()
	hm := kd.HealthMonitor
	kd.mu.Unlock()
	if hm == nil {
		return "unknown"
	}
	return hm.Health().String()
}

// WriterEpoch reports the in-band ratchet generation: the max writer epoch across
// both live directions, or 0 before a monitor exists. It snapshots the monitor
// under kd.mu, because teardown nils the field under the same lock; the snapshot's
// captured conns are immutable, so the epoch read itself is race-free.
func (kd *KeibiDrop) WriterEpoch() uint16 {
	kd.mu.Lock()
	hm := kd.HealthMonitor
	kd.mu.Unlock()
	if hm == nil {
		return 0
	}
	return hm.MaxWriterEpoch()
}

// ReconnectionState returns the current reconnection state. It snapshots the
// manager under kd.mu, because teardown nils the field under the same lock.
func (kd *KeibiDrop) ReconnectionState() string {
	kd.mu.Lock()
	rm := kd.ReconnectManager
	kd.mu.Unlock()
	if rm == nil {
		return "unknown"
	}
	return rm.State().String()
}

// ReconnectionAttempts returns the number of reconnection attempts.
func (kd *KeibiDrop) ReconnectionAttempts() int {
	if kd.ReconnectManager == nil {
		return 0
	}
	return kd.ReconnectManager.Attempts()
}
