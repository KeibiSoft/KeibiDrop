// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
	"strings"
	"sync"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
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
// Returns the local path where the file was saved.
func (api *API) SaveFile(remoteName string) (string, error) {
	if api.kd == nil {
		return "", fmt.Errorf("not initialized")
	}
	localPath := filepath.Join(api.savePath, remoteName)
	if err := api.ExportFile(remoteName, localPath); err != nil {
		return "", err
	}
	return localPath, nil
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

// --- File lists (index-based for gomobile compatibility) ---

// GetRemoteFileCount returns the number of files the peer is sharing.
func (api *API) GetRemoteFileCount() int {
	if api.kd == nil || api.kd.SyncTracker == nil {
		return 0
	}
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	defer api.kd.SyncTracker.RemoteFilesMu.RUnlock()
	return len(api.kd.SyncTracker.RemoteFiles)
}

// GetRemoteFileName returns the name of the remote file at index i.
func (api *API) GetRemoteFileName(i int) string {
	if api.kd == nil || api.kd.SyncTracker == nil {
		return ""
	}
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	defer api.kd.SyncTracker.RemoteFilesMu.RUnlock()
	idx := 0
	for name := range api.kd.SyncTracker.RemoteFiles {
		if idx == i {
			return name
		}
		idx++
	}
	return ""
}

// GetRemoteFileSize returns the size in bytes of the remote file at index i.
func (api *API) GetRemoteFileSize(i int) int64 {
	if api.kd == nil || api.kd.SyncTracker == nil {
		return 0
	}
	api.kd.SyncTracker.RemoteFilesMu.RLock()
	defer api.kd.SyncTracker.RemoteFilesMu.RUnlock()
	idx := 0
	for _, f := range api.kd.SyncTracker.RemoteFiles {
		if idx == i {
			return int64(f.Size)
		}
		idx++
	}
	return 0
}

// GetLocalFileCount returns the number of files you are sharing.
func (api *API) GetLocalFileCount() int {
	if api.kd == nil || api.kd.SyncTracker == nil {
		return 0
	}
	api.kd.SyncTracker.LocalFilesMu.RLock()
	defer api.kd.SyncTracker.LocalFilesMu.RUnlock()
	return len(api.kd.SyncTracker.LocalFiles)
}

// GetLocalFileName returns the name of the local file at index i.
func (api *API) GetLocalFileName(i int) string {
	if api.kd == nil || api.kd.SyncTracker == nil {
		return ""
	}
	api.kd.SyncTracker.LocalFilesMu.RLock()
	defer api.kd.SyncTracker.LocalFilesMu.RUnlock()
	idx := 0
	for name := range api.kd.SyncTracker.LocalFiles {
		if idx == i {
			return name
		}
		idx++
	}
	return ""
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
	bmPath := filesystem.BitmapPath(localPath)
	_, err := os.Stat(bmPath)
	return err == nil
}

// RelayEndpoint returns the relay URL string.
func (api *API) RelayEndpoint() string {
	if api.kd == nil {
		return ""
	}
	return api.kd.RelayEndoint.String()
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
