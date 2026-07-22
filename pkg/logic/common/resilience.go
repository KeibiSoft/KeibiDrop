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
	"strconv"
	"strings"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// InitConnectionResilience sets up health monitoring, reconnection, and relay keepalive.
// Call this after the session is connected and gRPC client is ready.
// newConfiguredHealthMonitor builds a fully-wired health monitor. BOTH the initial setup and the
// post-reconnect rebuild go through here so their configuration can never drift: a divergence
// between the two (an explicit RekeyEnabled override on the rebuild) silently dropped the near-wrap
// re-handshake once already. RekeyEnabled is the constructor default; onRekeyNeeded owns the
// actual rotation decision.
func (kd *KeibiDrop) newConfiguredHealthMonitor(sess *session.Session, client bindings.KeibiServiceClient, logger *slog.Logger) *session.HealthMonitor {
	hm := session.NewHealthMonitor(sess, client, kd.logger)
	hm.OnDisconnect = kd.onDisconnect
	hm.OnRekeyNeeded = kd.onRekeyNeeded
	// Near-wrap rescue must watch the QUIC lane too: it ratchets independently of the
	// captured TCP pair, and the rescue re-handshake re-derives the QUIC keys as well.
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

	// Debug-only: KD_REKEY_BYTES/KD_REKEY_MSGS lower the rotation thresholds so tests and
	// benchmarks can force a rekey without moving a gigabyte. Clamped to a floor (can only
	// fire sooner, never disable), logged once. Env-only, never persisted.
	if b, m := envRekeyUint("KD_REKEY_BYTES"), envRekeyUint("KD_REKEY_MSGS"); b != 0 || m != 0 {
		ab, am := session.ApplyRekeyThresholdOverride(b, m)
		logger.Warn("DEBUG rekey threshold override active (testing only)", "bytes", ab, "msgs", am)
	}

	// Initialize health monitor. RekeyEnabled defaults on in NewHealthMonitor (so the monitor
	// onReconnected rebuilds can't silently drop it); onRekeyNeeded decides whether a proposed
	// rotation actually fires, deferring to the ratchet except near the epoch-wrap guard.
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
		// Latent nil-deref (Andrei flagged it): if a prior bridge fallback closed+nil'd the
		// listener and we returned before reopening, Accept() on a nil interface panics.
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

// rekeyCooldown bounds how often a proactive rekey may fire, so a stuck reconnect
// cannot thrash the connection.
const rekeyCooldown = 60 * time.Second

// maxDisconnectDeferrals caps how many consecutive health-failure disconnects the
// active-transfer guard may defer. A transfer blocked on a dead connection never
// finishes on its own, so an unbounded deferral deadlocks recovery; at the cap the
// reconnect proceeds, the dead conn's blocked streams error out, and resumption
// picks the transfer back up.
const maxDisconnectDeferrals = 3

// envRekeyUint reads an unsigned-integer env var for the debug rekey threshold override
// (KD_REKEY_BYTES / KD_REKEY_MSGS). It returns 0 when unset, empty, or unparseable, which
// leaves the corresponding threshold at its production default.
func envRekeyUint(name string) uint64 {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// onRekeyNeeded rotates the session keys for forward secrecy when the connection is idle
// and has passed the data/message threshold. It performs a full re-handshake by dropping
// the current sockets and letting the proven reconnect path re-establish them with fresh
// per-direction keys. Only the deterministic reconnect-initiator drives it; the peer
// follows via its normal disconnect recovery. Gated on idle, so no transfer is cut.
// onRekeyNeeded returns true only when it actually initiates a rotation, so the health
// monitor knows whether it still owes a heartbeat this tick.
func (kd *KeibiDrop) onRekeyNeeded() bool {
	// F2: snapshot the session and reconnect-manager pointers once (under kd.mu) so a
	// concurrent teardown (Run's ctx.Done path nils kd.session / kd.ReconnectManager)
	// cannot flip either to nil between the nil-check and a later deref and panic us.
	// Every deref below uses the locals. Mirrors openStreamProvider, which snapshots
	// kd.session for the very same shutdown race.
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
	// The in-band ratchet rotates a new-new pair zero-pause, so the re-handshake rekey is the
	// fallback for old peers only. Dropping the sockets while the ratchet is on would be a
	// pointless stall, so defer to it, EXCEPT near the epoch-wrap guard: there the ratchet
	// stops advancing, so even a key-update pair must re-handshake for a fresh epoch-0 key.
	// Read after the reconnect-state guard: PeerSupportsKeyUpdate is rewritten by a reconnect
	// handshake, which only runs while state is Reconnecting, so this read lands in the same
	// reconnect-excluded zone as onRekeyNeeded's other session reads.
	if sess.UseKeyUpdate() && !sess.NearEpochWrap() {
		return false
	}

	// F3: never rekey while a transfer is in flight. A serving/on-demand transfer can
	// trickle under the health monitor's idle gate; dropping the sockets here would cut
	// it mid-stream. Mirrors onDisconnect's guard.
	if kd.hasActiveTransfers() {
		return false
	}

	// #6: never rekey while a REMOVE/RENAME notify is queued or in flight. The notify
	// worker has no retry queue and notifyRestoredFiles replays only ADD_FILE, so a drop
	// here would silently lose the delete/rename and diverge the peers. pendingNotifies
	// counts only from dequeue, so also check the channel for a just-enqueued one.
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
	if sess.Session != nil {
		if sess.Session.Inbound != nil {
			_ = sess.Session.Inbound.Close()
		}
		if sess.Session.Outbound != nil {
			_ = sess.Session.Outbound.Close()
		}
	}
	rm.OnDisconnect()
	return true
}

// onDisconnect is called by health monitor when connection is lost.
func (kd *KeibiDrop) onDisconnect() {
	logger := kd.logger.With("event", "disconnect")

	// Don't tear down the connection while transfers are active.
	// Heartbeat failures during large transfers are expected (the gRPC
	// connection is saturated with file data, starving heartbeat RPCs).
	// BOUNDED (maxDisconnectDeferrals): a transfer stuck on a dead connection holds the active count
	// forever, and an unbounded deferral then starves reconnection while the
	// stuck transfer waits for exactly the reconnect being deferred (observed
	// as a test-suite hang in TestUXLatency_LiveEditPropagation). After the
	// cap we reconnect anyway; tearing the dead conn errors the blocked
	// streams out and resumption recovers the transfer.
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

	// Snapshot the resilience handles under kd.mu; teardown nils them concurrently.
	// Call their methods outside the lock so kd.mu is never held across a
	// RelayKeepalive/ReconnectManager call (keeps the rk.mu -> kd.mu order intact).
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

// onReconnected is called when reconnection succeeds.
// It rebuilds the gRPC client and server on the fresh P2P sockets so that
// all subsequent RPCs (file ops, health checks) use the new connection.
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

	// Stop old health monitor BEFORE tearing down gRPC so it doesn't
	// fire heartbeats against the dead client and trigger a spurious
	// second reconnection.
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

	// Teardown may have begun during the 15s connect above; don't resurrect a health
	// monitor on a session Run is tearing down (that leaks a goroutine on a dead
	// session). The guard bounds the window; teardown set tearingDown before niling.
	if kd.tearingDown.Load() {
		logger.Info("Reconnect finished during shutdown; skipping monitor restart")
		return
	}

	// The re-handshake derived fresh QUIC keys (SEK*QUIC), so the old QUIC control
	// pair is cryptographically dead — restart it (idempotent: stops the old one).
	kd.StartQUICControlChannel()

	// Restart health monitoring with the fresh client. kd.KDClient was just set by the
	// connect above on this goroutine; snapshot kd.session (teardown writes it under
	// kd.mu) and publish the new monitor under the lock.
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
	// Snapshot kd.session under kd.mu: this runs as a detached goroutine spawned by
	// onReconnected, so Run's ctx.Done teardown can nil kd.session concurrently. The
	// lock pairs with the teardown's locked write for a clean happens-before.
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

// WriterEpoch reports the current in-band ratchet generation (the max writer epoch across both
// live directions), or 0 before a monitor/connection exists. Pull-based and race-free (it reads
// the monitor's captured conns), so `kd status` can surface it for smoke tests and kd-bench to
// watch the ratchet advance without any log spam.
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
