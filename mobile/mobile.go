// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Package mobile exposes a gomobile-compatible API for iOS and Android clients
// ABOUTME: All types use gomobile-safe primitives; file lists use index-based snapshot access

// Package mobile provides gomobile-compatible bindings for KeibiDrop.
// All exported types and functions follow gomobile restrictions:
// no maps, no slices (except []byte), no channels, no complex types.
// File lists use index-based access (GetRemoteFileCount + GetRemoteFileName).
package mobile

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/discovery"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// API is the main entry point for mobile apps.
// Create one instance, call Initialize, then Start in a background thread.
type API struct {
	kd               *common.KeibiDrop
	running          bool
	ctxCancel        context.CancelFunc
	logger           *slog.Logger
	op               *opState
	opTimeoutSeconds int
	savePath         string

	// Event channel for health/connection events.
	eventCh   chan string
	eventOnce sync.Once

	// File list snapshots (taken by RefreshFileList, read by getters).
	remoteSnap fileSnapshot
	localSnap  fileSnapshot

	// LAN discovery
	disc *discovery.Service

	mu sync.Mutex
}

// Initialize sets up the KeibiDrop engine. Call once before Start.
// savePath is where received files are stored (app sandbox directory).
// No FUSE on mobile. prefetchOnOpen and pushOnWrite are ignored.
func (api *API) Initialize(logFilePath string, relayURL string, inboundPort int, outboundPort int, savePath string) error {
	var wr *os.File = os.Stderr
	if logFilePath != "" {
		f, err := os.OpenFile(filepath.Clean(logFilePath),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			slog.Warn("Failed to open log file, defaulting to stderr",
				"path", logFilePath, "error", err)
		} else {
			wr = f
		}
	}

	handler := slog.NewTextHandler(wr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	})
	logger := slog.New(handler).With("component", "mobile")

	parsedURL, err := url.Parse(relayURL)
	if err != nil {
		return fmt.Errorf("invalid relay URL: %w", err)
	}

	if savePath != "" {
		if err := os.MkdirAll(savePath, 0755); err != nil {
			return fmt.Errorf("create save path: %w", err)
		}
	}

	kdctx, cancel := context.WithCancel(context.Background())
	kd, err := common.NewKeibiDrop(kdctx, logger, false, parsedURL, inboundPort, outboundPort, "", savePath, false, false)
	if err != nil {
		cancel()
		return fmt.Errorf("init failed: %w", err)
	}

	// Default bridge relay for mobile peers — direct P2P rarely works on mobile
	// (NAT, carrier-grade NAT, no inbound ports). The bridge is the primary path.
	kd.BridgeAddr = "bridge.keibisoft.com:26600"

	api.mu.Lock()
	api.ctxCancel = cancel
	api.kd = kd
	api.logger = logger
	api.running = false
	api.savePath = savePath
	api.op = newOpState()
	api.opTimeoutSeconds = 10 * 60
	api.eventCh = make(chan string, 64)
	api.mu.Unlock()

	return nil
}

// SetBridgeAddr overrides the default bridge relay address.
func (api *API) SetBridgeAddr(addr string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.kd != nil {
		api.kd.BridgeAddr = addr
	}
}

// Start runs the KeibiDrop event loop. Blocks until Stop is called.
// Call this from a background thread on mobile.
func (api *API) Start() error {
	api.mu.Lock()
	if api.kd == nil {
		api.mu.Unlock()
		return fmt.Errorf("not initialized")
	}
	if api.running {
		api.mu.Unlock()
		return fmt.Errorf("already running")
	}
	api.running = true
	api.mu.Unlock()

	go api.kd.Run()

	// Block until stopped.
	for {
		time.Sleep(time.Second)
		api.mu.Lock()
		r := api.running
		api.mu.Unlock()
		if !r {
			return nil
		}
	}
}

// Stop shuts down the engine. Safe to call multiple times.
func (api *API) Stop() error {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.kd == nil {
		return nil
	}
	if api.ctxCancel != nil {
		api.ctxCancel()
	}
	api.running = false
	return nil
}

// Disconnect ends the current session but keeps the engine alive.
// You can CreateRoom/JoinRoom again after this.
func (api *API) Disconnect() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	api.kd.NotifyDisconnect()
	api.kd.Stop()
	return nil
}

// --- Peer exchange ---

// Fingerprint returns your local fingerprint code.
// Share this with your peer via Signal, Telegram, email, etc.
func (api *API) Fingerprint() (string, error) {
	if api.kd == nil {
		return "", fmt.Errorf("not initialized")
	}
	return api.kd.ExportFingerprint()
}

// RegisterPeer registers the peer's fingerprint code.
func (api *API) RegisterPeer(fingerprint string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.AddPeerFingerprint(fingerprint)
}

// PeerFingerprint returns the registered peer's fingerprint, or empty string.
func (api *API) PeerFingerprint() string {
	if api.kd == nil {
		return ""
	}
	fp, _ := api.kd.GetPeerFingerprint()
	return fp
}

// --- Room operations ---

// CreateRoom creates a room and waits for the peer to join. Blocking.
func (api *API) CreateRoom() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.CreateRoom()
}

// JoinRoom joins a room created by the peer. Blocking.
func (api *API) JoinRoom() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.JoinRoom()
}

// CreateRoomAsync starts room creation in the background.
// Poll GetOpStatus() to check progress.
func (api *API) CreateRoomAsync() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	if api.op == nil {
		api.op = newOpState()
	}
	status, _ := api.op.get()
	if status == OpStatusRunning {
		return fmt.Errorf("operation already in progress")
	}
	api.op.set(OpStatusRunning, "creating room")
	go func() {
		if err := api.CreateRoom(); err != nil {
			api.op.set(OpStatusFailed, err.Error())
			return
		}
		api.op.set(OpStatusSucceeded, "connected")
	}()
	return nil
}

// JoinRoomAsync starts joining a room in the background.
// Poll GetOpStatus() to check progress.
func (api *API) JoinRoomAsync() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	if api.op == nil {
		api.op = newOpState()
	}
	status, _ := api.op.get()
	if status == OpStatusRunning {
		return fmt.Errorf("operation already in progress")
	}
	api.op.set(OpStatusRunning, "joining room")
	go func() {
		if err := api.JoinRoom(); err != nil {
			api.op.set(OpStatusFailed, err.Error())
			return
		}
		api.op.set(OpStatusSucceeded, "connected")
	}()
	return nil
}

// Connect determines the creator/joiner role automatically via
// fingerprint comparison, then creates or joins a room. Blocking.
func (api *API) Connect() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.Connect()
}

// ConnectAsync starts Connect in the background.
// Poll GetOpStatus() to check progress.
func (api *API) ConnectAsync() error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	if api.op == nil {
		api.op = newOpState()
	}
	status, _ := api.op.get()
	if status == OpStatusRunning {
		return fmt.Errorf("operation already in progress")
	}
	api.op.set(OpStatusRunning, "connecting")
	go func() {
		if err := api.Connect(); err != nil {
			api.op.set(OpStatusFailed, err.Error())
			return
		}
		api.op.set(OpStatusSucceeded, "connected")
	}()
	return nil
}

// CancelOp cancels the current async operation.
func (api *API) CancelOp() error {
	if api.op == nil {
		return fmt.Errorf("no operation")
	}
	api.op.set(OpStatusFailed, "cancelled")
	return api.Disconnect()
}

// GetOpStatus returns the status of the current async operation.
func (api *API) GetOpStatus() *OpStatus {
	if api.op == nil {
		return &OpStatus{Status: OpStatusIdle}
	}
	status, msg := api.op.get()
	if status == OpStatusRunning {
		timeout := api.opTimeoutSeconds
		if timeout == 0 {
			timeout = 600
		}
		api.op.mu.Lock()
		start := api.op.startedAt
		api.op.mu.Unlock()
		if time.Since(start) > time.Duration(timeout)*time.Second {
			api.op.set(OpStatusTimeout, "timed out")
			status, msg = api.op.get()
		}
	}
	return &OpStatus{Status: status, Message: msg}
}

// --- File operations ---

// ImportFile adds a local file to share with the peer.
// localPath is a path to a file on the device (from file picker, Photos, etc).
func (api *API) ImportFile(localPath string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.AddFile(localPath)
}

// ImportFileAs adds a local file with a custom remote name (preserving folder paths).
func (api *API) ImportFileAs(localPath string, remoteName string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.AddFileAs(localPath, remoteName)
}

// ExportFile downloads a file from the peer and saves it to destPath.
// destPath is where to write on the device (app sandbox, then share via OS).
func (api *API) ExportFile(remoteName string, destPath string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	if dir := filepath.Dir(destPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	}
	return api.kd.PullFile(remoteName, destPath)
}

// SaveFile downloads a file from the peer into the save folder.
// If the file already exists at the correct size, it skips the download.
// Returns the local path where the file was saved.
func (api *API) SaveFile(remoteName string) (string, error) {
	if api.kd == nil {
		return "", fmt.Errorf("not initialized")
	}
	localPath := filepath.Join(api.savePath, remoteName)

	// Check if already downloaded at correct size.
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	rf, ok := api.kd.SyncTracker.RemoteFiles[remoteName]
	var expectedSize uint64
	if ok {
		expectedSize = rf.Size
	}
	api.kd.SyncTracker.RemoteFilesMu.RUnlock()

	if info, statErr := os.Stat(localPath); statErr == nil {
		if expectedSize > 0 && uint64(info.Size()) == expectedSize {
			// Already have the full file, no need to re-download.
			return localPath, nil
		}
	}

	if err := api.ExportFile(remoteName, localPath); err != nil {
		return "", err
	}
	return localPath, nil
}

// SaveAllFiles downloads all remote files to the save folder sequentially.
// Returns the number of files successfully saved.
func (api *API) SaveAllFiles() int {
	api.mu.Lock()
	kd := api.kd
	api.mu.Unlock()
	if kd == nil || kd.SyncTracker == nil {
		return 0
	}

	kd.SyncTracker.RemoteFilesMu.RLock()
	names := make([]string, 0, len(kd.SyncTracker.RemoteFiles))
	for name := range kd.SyncTracker.RemoteFiles {
		if !isInternalFile(name) {
			names = append(names, name)
		}
	}
	kd.SyncTracker.RemoteFilesMu.RUnlock()

	saved := 0
	for _, name := range names {
		if _, err := api.SaveFile(name); err == nil {
			saved++
		}
	}
	return saved
}

// CancelDownload stops an active download. Partial data is preserved.
// Call SaveFile or ExportFile again to resume.
func (api *API) CancelDownload(remoteName string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	return api.kd.CancelDownload(remoteName)
}

// GetDownloadProgress returns download progress as 0-100, or -1 if not active.
func (api *API) GetDownloadProgress(remoteName string) int {
	if api.kd == nil {
		return -1
	}
	p := api.kd.GetDownloadProgress(remoteName)
	if p < 0 {
		return -1
	}
	return int(p * 100)
}

// isInternalFile returns true for macOS/FUSE internal files that should not appear in the UI.
func isInternalFile(name string) bool {
	return strings.Contains(name, ".fuse_hidden") ||
		strings.Contains(name, ".fseventsd") ||
		strings.Contains(name, ".fseventuuid") ||
		strings.Contains(name, ".DS_Store") ||
		strings.Contains(name, ".kdbitmap")
}

// --- File lists (index-based for gomobile compatibility) ---
//
// Snapshots are taken once per poll cycle to avoid races between
// count/name/size calls. The snapshot is protected by api.mu.

type fileSnapshot struct {
	names []string
	sizes []int64
}

func (api *API) snapshotRemoteFiles() {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.kd == nil || api.kd.SyncTracker == nil {
		api.remoteSnap = fileSnapshot{}
		return
	}
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	snap := fileSnapshot{
		names: make([]string, 0, len(api.kd.SyncTracker.RemoteFiles)),
		sizes: make([]int64, 0, len(api.kd.SyncTracker.RemoteFiles)),
	}
	for name, f := range api.kd.SyncTracker.RemoteFiles {
		if isInternalFile(name) {
			continue
		}
		snap.names = append(snap.names, name)
		snap.sizes = append(snap.sizes, int64(f.Size))
	}
	api.kd.SyncTracker.RemoteFilesMu.RUnlock()
	// Sort for stable ordering.
	sort.Slice(snap.names, func(i, j int) bool {
		return snap.names[i] < snap.names[j]
	})
	// Re-sort sizes to match name order.
	nameToSize := make(map[string]int64, len(snap.names))
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	for name, f := range api.kd.SyncTracker.RemoteFiles {
		nameToSize[name] = int64(f.Size)
	}
	api.kd.SyncTracker.RemoteFilesMu.RUnlock()
	for i, name := range snap.names {
		snap.sizes[i] = nameToSize[name]
	}
	api.remoteSnap = snap
}

func (api *API) snapshotLocalFiles() {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.kd == nil || api.kd.SyncTracker == nil {
		api.localSnap = fileSnapshot{}
		return
	}
	api.kd.SyncTracker.LocalFilesMu.RLock()
	snap := fileSnapshot{
		names: make([]string, 0, len(api.kd.SyncTracker.LocalFiles)),
	}
	for name := range api.kd.SyncTracker.LocalFiles {
		if isInternalFile(name) {
			continue
		}
		snap.names = append(snap.names, name)
	}
	api.kd.SyncTracker.LocalFilesMu.RUnlock()
	sort.Strings(snap.names)
	api.localSnap = snap
}

// RefreshFileList takes a consistent snapshot of remote and local files.
// Call this once per poll cycle, then use the count/name/size getters.
func (api *API) RefreshFileList() {
	api.snapshotRemoteFiles()
	api.snapshotLocalFiles()
}

// GetRemoteFileCount returns the number of remote files in the last snapshot.
func (api *API) GetRemoteFileCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.remoteSnap.names)
}

// GetRemoteFileName returns the name of the remote file at index i in the
// last snapshot.  Returns empty string if i is out of bounds.
func (api *API) GetRemoteFileName(i int) string {
	api.mu.Lock()
	defer api.mu.Unlock()
	if i < 0 || i >= len(api.remoteSnap.names) {
		return ""
	}
	return api.remoteSnap.names[i]
}

// GetRemoteFileSize returns the size in bytes of the remote file at index i
// in the last snapshot.  Returns 0 if i is out of bounds.
func (api *API) GetRemoteFileSize(i int) int64 {
	api.mu.Lock()
	defer api.mu.Unlock()
	if i < 0 || i >= len(api.remoteSnap.sizes) {
		return 0
	}
	return api.remoteSnap.sizes[i]
}

// GetLocalFileCount returns the number of local files in the last snapshot.
func (api *API) GetLocalFileCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	return len(api.localSnap.names)
}

// GetLocalFileName returns the name of the local file at index i in the
// last snapshot.  Returns empty string if i is out of bounds.
func (api *API) GetLocalFileName(i int) string {
	api.mu.Lock()
	defer api.mu.Unlock()
	if i < 0 || i >= len(api.localSnap.names) {
		return ""
	}
	return api.localSnap.names[i]
}

// --- Connection status ---

// GetConnectionStatus returns 0=disconnected, 2=connected, 3=reconnecting.
func (api *API) GetConnectionStatus() int {
	if api.kd == nil {
		return 0
	}
	if api.kd.HealthMonitor == nil {
		return 2 // no monitor = assume connected
	}
	switch api.kd.HealthMonitor.Health() {
	case 0:
		return 2 // connected
	case 1:
		return 3 // reconnecting
	default:
		return 0 // disconnected
	}
}

// --- Events ---

// SetupEventCallbacks wires health/reconnect events into the event queue.
// Call once after Initialize, before Start.
func (api *API) SetupEventCallbacks() {
	if api.kd == nil {
		return
	}
	api.eventOnce.Do(func() {
		api.kd.OnEvent = func(event string) {
			select {
			case api.eventCh <- event:
			default:
				// Channel full, drop oldest.
			}
		}
	})
}

// PollEvent returns the next event string, or empty string if none.
// Non-blocking. Event format: "type:payload" (e.g. "health_changed:healthy:disconnected").
func (api *API) PollEvent() string {
	select {
	case ev := <-api.eventCh:
		return ev
	default:
		return ""
	}
}

// --- Info ---

// GetVersion returns the version string.
func (api *API) GetVersion() string {
	return common.Version + " (" + common.CommitHash + ")"
}

// GetLastError returns the last error message, or empty string.
func (api *API) GetLastError() string {
	if api.kd == nil {
		return ""
	}
	// Mobile API surfaces errors via return values.
	// This is a convenience for checking async operation errors.
	if api.op != nil {
		status, msg := api.op.get()
		if status == OpStatusFailed {
			return msg
		}
	}
	return ""
}

// GetSavePath returns the configured save folder path.
func (api *API) GetSavePath() string {
	return api.savePath
}

// HasResumableDownload returns true if a .kdbitmap file exists for the given file,
// meaning a previous download was interrupted and can be resumed.
func (api *API) HasResumableDownload(remoteName string) bool {
	localPath := filepath.Join(api.savePath, remoteName)
	_, err := os.Stat(localPath + ".kdbitmap")
	return err == nil
}

// GetLocalAddress returns the link-local IPv6 address for local mode connections.
// Format: "fe80::1234:5678%en0:26431"
func (api *API) GetLocalAddress() string {
	port := 26431 // default inbound port
	if api.kd != nil {
		port = api.kd.InboundPort()
	}
	addr, err := common.GetLinkLocalAddress(port)
	if err != nil {
		return ""
	}
	return addr
}

// SetPeerDirectAddress sets the peer's address for local mode (no relay).
// Automatically enables local mode (skips relay, uses TOFU handshake).
func (api *API) SetPeerDirectAddress(addr string) error {
	if api.kd == nil {
		return fmt.Errorf("not initialized")
	}
	api.kd.IsLocalMode = true
	return api.kd.SetPeerDirectAddress(addr)
}

// GetConnectionMode returns the current connection mode: "lan", "direct", "bridge", or "" if not connected.
func (api *API) GetConnectionMode() string {
	if api.kd == nil {
		return ""
	}
	return api.kd.ConnectionMode
}

// SetStrictMode enables/disables strict mode (no data relay fallback).
func (api *API) SetStrictMode(enabled bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.kd != nil {
		api.kd.StrictMode = enabled
	}
}

// RelayEndpoint returns the relay URL string.
func (api *API) RelayEndpoint() string {
	if api.kd == nil {
		return ""
	}
	return api.kd.RelayEndoint.String()
}

// --- LAN Discovery ---

// StartDiscovery begins broadcasting presence and listening for peers on the LAN.
// Also enables local mode (skips relay for connections).
func (api *API) StartDiscovery() {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc != nil {
		return
	}
	port := 26431
	if api.kd != nil {
		port = api.kd.InboundPort()
		api.kd.IsLocalMode = true
	}
	logger := api.logger
	if logger == nil {
		logger = slog.Default()
	}
	api.disc = discovery.New(port, logger)
	_ = api.disc.Start()
}

// StopDiscovery stops LAN discovery and disables local mode.
func (api *API) StopDiscovery() {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc != nil {
		api.disc.Stop()
		api.disc = nil
	}
	if api.kd != nil {
		api.kd.IsLocalMode = false
	}
}

// GetDiscoveryName returns this device's random display name for LAN discovery.
func (api *API) GetDiscoveryName() string {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc == nil {
		return ""
	}
	return api.disc.Name()
}

// GetDiscoveredPeerCount returns the number of discovered peers on the LAN.
func (api *API) GetDiscoveredPeerCount() int {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc == nil {
		return 0
	}
	return len(api.disc.Peers())
}

// GetDiscoveredPeerName returns the name of the discovered peer at index i.
func (api *API) GetDiscoveredPeerName(i int) string {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc == nil {
		return ""
	}
	peers := api.disc.Peers()
	if i < 0 || i >= len(peers) {
		return ""
	}
	return peers[i].Name
}

// GetDiscoveredPeerAddr returns the address of the discovered peer at index i.
func (api *API) GetDiscoveredPeerAddr(i int) string {
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.disc == nil {
		return ""
	}
	peers := api.disc.Peers()
	if i < 0 || i >= len(peers) {
		return ""
	}
	return peers[i].Addr
}

// FormatFileSize returns a human-readable file size string.
func FormatFileSize(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(1<<30))
	case bytes >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(bytes)/float64(1<<20))
	case bytes >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(bytes)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

// FileTypeFromName returns a category string for a file based on its extension.
// Returns one of: "pdf", "image", "video", "audio", "code", "archive", "text", "other".
func FileTypeFromName(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".pdf"):
		return "pdf"
	case strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") ||
		strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".gif") ||
		strings.HasSuffix(lower, ".webp") || strings.HasSuffix(lower, ".heic"):
		return "image"
	case strings.HasSuffix(lower, ".mp4") || strings.HasSuffix(lower, ".mov") ||
		strings.HasSuffix(lower, ".avi") || strings.HasSuffix(lower, ".mkv") ||
		strings.HasSuffix(lower, ".webm"):
		return "video"
	case strings.HasSuffix(lower, ".mp3") || strings.HasSuffix(lower, ".wav") ||
		strings.HasSuffix(lower, ".flac") || strings.HasSuffix(lower, ".aac") ||
		strings.HasSuffix(lower, ".m4a"):
		return "audio"
	case strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar") ||
		strings.HasSuffix(lower, ".gz") || strings.HasSuffix(lower, ".7z") ||
		strings.HasSuffix(lower, ".rar"):
		return "archive"
	case strings.HasSuffix(lower, ".go") || strings.HasSuffix(lower, ".rs") ||
		strings.HasSuffix(lower, ".py") || strings.HasSuffix(lower, ".js") ||
		strings.HasSuffix(lower, ".ts") || strings.HasSuffix(lower, ".swift") ||
		strings.HasSuffix(lower, ".kt") || strings.HasSuffix(lower, ".java"):
		return "code"
	case strings.HasSuffix(lower, ".txt") || strings.HasSuffix(lower, ".md") ||
		strings.HasSuffix(lower, ".csv") || strings.HasSuffix(lower, ".json") ||
		strings.HasSuffix(lower, ".xml") || strings.HasSuffix(lower, ".yaml"):
		return "text"
	default:
		return "other"
	}
}
