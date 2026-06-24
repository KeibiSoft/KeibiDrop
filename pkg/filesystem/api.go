//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

type FS struct {
	logger *slog.Logger

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	// Collab sync options (set from env before Mount).
	PrefetchOnOpen    bool // If true, fetch entire file on Open() and write to local disk.
	PrefetchAutoMB    int  // files >= this many MB auto-prefetch on open (0=off; PrefetchOnOpen forces any size)
	ReadAheadWindowMB int  // cap (MB) for predictive sequential read-ahead on on-demand reads (0=off). Converted to blocks per Dir.
	PushOnWrite       bool // If true, async push deltas to peer on Write().
	AutoCache         bool // If true (live_collab), add the macFUSE auto_cache mount option so a peer's same-size in-place edit is seen live. Trades off mmap-write integrity (git) on macOS; no-op on Linux/Windows.

	// Host.
	host *winfuse.FileSystemHost
	Root *Dir

	// Cancelled during Unmount to unblock FUSE handlers waiting on gRPC.
	ctx        context.Context
	cancel     context.CancelFunc
	mountPoint string

	// cacheOwnerFP is the actual fingerprint of the peer whose files are currently
	// cached/shown. Connecting to a DIFFERENT peer drops the cache first (so a
	// peer's artifacts never leak to the next), while the SAME peer reconnecting or
	// resuming a session keeps the cache — no re-fetch, files still there.
	cacheOwnerFP string
	cacheOwnerMu sync.Mutex
}

func NewFS(logger *slog.Logger) *FS {
	ctx, cancel := context.WithCancel(context.Background())
	return &FS{
		logger: logger,
		ctx:    ctx,
		cancel: cancel,
	}
}

// Mount blocks for the lifetime of the FUSE session. It returns an error
// immediately if the mount point is invalid or host.Mount() refuses to come
// up; on success it returns nil only after a clean unmount.
func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) error {
	fs.logger.Warn("FUSE Mount starting",
		"mountPoint", mountPoint,
		"downloadPath", downloadPath,
		"isSecond", isSecond)

	cleanMountPoint := filepath.Clean(mountPoint)

	// Clean up stale FUSE mounts from a previous crash.
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(cleanMountPoint); err == nil {
			// Try reading the directory. If it fails with "device not configured",
			// the mount point has a stale FUSE mount that needs force-unmounting.
			if _, readErr := os.ReadDir(cleanMountPoint); readErr != nil {
				fs.logger.Warn("Stale FUSE mount detected, force-unmounting",
					"mountPoint", cleanMountPoint, "error", readErr)
				if runtime.GOOS == "darwin" {
					_ = exec.Command("diskutil", "unmount", "force", cleanMountPoint).Run() // #nosec G204 -- mount point from config
				} else {
					_ = exec.Command("fusermount", "-u", cleanMountPoint).Run() // #nosec G204 -- mount point from config
				}
				time.Sleep(500 * time.Millisecond)
			}
		}
	}

	// On Windows, normalise drive-letter mount points for WinFSP:
	//   "K:."  (filepath.Clean("K:"))  → "K:"
	//   "K:\"  (filepath.Clean("K:\")) → "K:"
	// WinFSP expects a bare drive letter without a trailing separator.
	if runtime.GOOS == "windows" && len(cleanMountPoint) >= 2 && cleanMountPoint[1] == ':' {
		stripped := cleanMountPoint[:2]
		if len(cleanMountPoint) == 3 && (cleanMountPoint[2] == '.' || cleanMountPoint[2] == '\\' || cleanMountPoint[2] == '/') {
			cleanMountPoint = stripped
		}
	}
	if cleanMountPoint == "" || cleanMountPoint == "." {
		fs.logger.Warn("FUSE Mount failed - invalid mount point", "mountPoint", mountPoint)
		return fmt.Errorf("invalid mount point %q", mountPoint)
	}

	// WinFSP rejects an existing mount path; clear a stale empty dir left by a crash or
	// older build. os.Remove only deletes an empty dir, so no data is lost.
	if isWindowsDirMountPoint(runtime.GOOS, cleanMountPoint) {
		if fi, statErr := os.Stat(cleanMountPoint); statErr == nil && fi.IsDir() {
			_ = os.Remove(cleanMountPoint)
		}
	}

	root := &Dir{
		logger: fs.logger.With("mount", "root"),
		Inode:  0,
		Name:   "",

		RelativePath:   "/",
		RealPathOfFile: downloadPath,

		IsLocalPresent: true,
		PeerLastEdit:   0,
		Parent:         nil,

		// IDK about this one.
		LocalDownloadFolder: filepath.Clean(downloadPath),

		OpenMapLock:      sync.RWMutex{},
		OpenFileHandlers: make(map[uint64]*HandleEntry),

		Adm:       sync.RWMutex{},
		AllDirMap: make(map[string]*Dir),

		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*File),

		OnLocalChange:      fs.OnLocalChange,
		OpenStreamProvider: fs.OpenStreamProvider,

		PrefetchOnOpen:        fs.PrefetchOnOpen,
		PrefetchAutoMB:        fs.PrefetchAutoMB,
		ReadAheadWindowBlocks: readAheadWindowBlocks(fs.ReadAheadWindowMB),
		PushOnWrite:           fs.PushOnWrite,

		FsCtx: fs.ctx,

		RemoteFilesLock: sync.RWMutex{},
		RemoteFiles:     make(map[string]*File),

		PrefetchSem: make(chan struct{}, 8), // max 8 concurrent prefetches
	}

	root.Root = root
	fs.Root = root

	host := winfuse.NewFileSystemHost(root)
	host.SetCapReaddirPlus(true)
	host.SetUseIno(true)
	fs.host = host
	fs.mountPoint = cleanMountPoint

	opts := getMountOptions(fs.AutoCache)

	fs.logger.Warn("FUSE Mount calling host.Mount", "cleanMountPoint", cleanMountPoint, "opts", opts)
	ok := fs.host.Mount(cleanMountPoint, opts)
	if !ok {
		// Reset host/Root so IsMounted() reports false. Otherwise a failed mount
		// leaves them set, and the next reconnect takes the "already mounted,
		// waiting" branch and never retries — hanging the mount permanently.
		fs.host = nil
		fs.Root = nil
		fs.logger.Error("FUSE Mount failed", "mountPoint", cleanMountPoint)
		hint := "ensure user_allow_other is set in /etc/fuse.conf, or run with KD_NO_FUSE=1"
		if runtime.GOOS == "windows" {
			hint = "ensure WinFSP is installed and the mount point does not already exist, or run with KD_NO_FUSE=1"
		}
		return fmt.Errorf("FUSE mount failed for %s: %s", cleanMountPoint, hint)
	}
	fs.logger.Warn("FUSE Mount completed", "mountPoint", cleanMountPoint)
	return nil
}

// isWindowsDirMountPoint is true for a Windows mount path that is a dir, not a drive ("K:").
func isWindowsDirMountPoint(goos, p string) bool {
	return goos == "windows" && (len(p) != 2 || p[1] != ':')
}

func (fs *FS) Unmount() {
	fs.logger.Warn("FUSE Unmount starting", "hostNil", fs.host == nil)
	if fs.host == nil {
		fs.logger.Warn("FUSE Unmount skipped - host is nil")
		return
	}

	fs.cancel()

	if fs.Root != nil {
		fs.drainInFlightOperations()
	}

	done := make(chan struct{})
	go func() {
		fs.host.Unmount()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		fs.logger.Warn("FUSE Unmount timed out, force-unmounting", "mountPoint", fs.mountPoint)
		fs.forceUnmount()
		<-done
	}
	fs.Root = nil
	fs.logger.Warn("FUSE Unmount completed")
}

// RefreshCallbacks updates the Root's callbacks to match the FS-level ones.
// Call after setupFilesystem re-wires OnLocalChange/OpenStreamProvider for
// a new session, so the persistent Root uses the new session's gRPC client.
func (fs *FS) RefreshCallbacks() {
	if fs.Root != nil {
		fs.Root.OnLocalChange = fs.OnLocalChange
		fs.Root.OpenStreamProvider = fs.OpenStreamProvider
		fs.Root.FsCtx = fs.ctx
	}
}

// IsMounted returns true if the FUSE host is active.
func (fs *FS) IsMounted() bool {
	return fs.host != nil && fs.Root != nil
}

// ClearFiles cancels in-flight operations and clears all file/dir maps
// without unmounting. The mount stays alive as an empty folder. Used on
// disconnect so the same mount can be reused on reconnect (cgofuse only
// allows one mount per process).
func (fs *FS) ClearFiles() {
	fs.cancel()
	if fs.Root != nil {
		fs.drainInFlightOperations()
		fs.Root.AfmLock.Lock()
		fs.Root.AllFileMap = make(map[string]*File)
		fs.Root.AfmLock.Unlock()
		fs.Root.Adm.Lock()
		fs.Root.AllDirMap = make(map[string]*Dir)
		fs.Root.Adm.Unlock()
		fs.Root.RemoteFilesLock.Lock()
		fs.Root.RemoteFiles = make(map[string]*File)
		fs.Root.RemoteFilesLock.Unlock()
		fs.Root.OpenMapLock.Lock()
		fs.Root.OpenFileHandlers = make(map[uint64]*HandleEntry)
		fs.Root.OpenMapLock.Unlock()
	}
	// Fresh context for the next session.
	fs.ctx, fs.cancel = context.WithCancel(context.Background())
	if fs.Root != nil {
		fs.Root.FsCtx = fs.ctx
	}
}

// CancelInFlight cancels in-flight reads/prefetches and resets the FUSE context but
// PRESERVES the cached file view (maps + bitmaps). Used on a disconnect from a peer we
// may reconnect to or resume with: the same peer reconnecting finds its files still
// there and resumes on-demand with no re-fetch. The full ClearFiles wipe is reserved
// for switching to a DIFFERENT peer (see EnsurePeerScope).
func (fs *FS) CancelInFlight() {
	fs.cancel()
	if fs.Root != nil {
		fs.drainInFlightOperations()
	}
	// Fresh context so the next session's reads aren't born cancelled.
	fs.ctx, fs.cancel = context.WithCancel(context.Background())
	if fs.Root != nil {
		fs.Root.FsCtx = fs.ctx
	}
}

// EnsurePeerScope binds the cached file view to a peer identity. Connecting to a
// DIFFERENT peer than the one whose files are cached drops the cache first, so one
// peer's artifacts never leak to the next; the SAME peer (reconnect, or a later
// session) keeps it, so files are still there with no re-fetch. The first peer adopts
// the (empty) cache without clearing. Idempotent and cheap on repeat calls. fp is the
// peer's ACTUAL verified fingerprint, so this is correct in TOFU mode too.
func (fs *FS) EnsurePeerScope(fp string) {
	if fp == "" {
		return // unknown identity — never risk a spurious wipe
	}
	fs.cacheOwnerMu.Lock()
	defer fs.cacheOwnerMu.Unlock()
	if fs.cacheOwnerFP == fp {
		return // same peer — keep the cache
	}
	if fs.cacheOwnerFP != "" {
		fs.ClearFiles() // different peer — drop the previous peer's view
	}
	fs.cacheOwnerFP = fp
}

func (fs *FS) forceUnmount() {
	mp := fs.mountPoint
	if mp == "" {
		return
	}
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("diskutil", "unmount", "force", mp).Run() // #nosec G204
	case "linux":
		_ = exec.Command("fusermount", "-u", mp).Run() // #nosec G204
	case "windows":
		// WinFsp handles cleanup via host.Unmount(); no force path needed.
	}
}

// drainInFlightOperations cancels all prefetch goroutines and closes all
// stream pools on open file handles so that pending FUSE Read/Open handlers
// unblock immediately instead of waiting for gRPC timeouts.
func (fs *FS) drainInFlightOperations() {
	root := fs.Root
	if root == nil {
		return
	}

	// 1. Cancel all prefetch goroutines (they hold PrefetchSem slots and
	//    stream from the peer via gRPC).
	root.AfmLock.RLock()
	for _, f := range root.AllFileMap {
		if f.PrefetchCancel != nil {
			f.PrefetchCancel()
			f.PrefetchCancel = nil
		}
	}
	root.AfmLock.RUnlock()

	// 2. Close all stream pools on open file handles. This causes any
	//    in-flight pool.ReadAt() call (inside a FUSE Read handler) to
	//    return an error immediately instead of blocking on the 10s timeout.
	root.OpenMapLock.Lock()
	for _, entry := range root.OpenFileHandlers {
		if entry.File != nil {
			if entry.File.StreamPool != nil {
				_ = entry.File.StreamPool.Close()
				entry.File.StreamPool = nil
			}
			if entry.File.StreamCancel != nil {
				entry.File.StreamCancel()
				entry.File.StreamCancel = nil
			}
		}
	}
	root.OpenMapLock.Unlock()

	fs.logger.Warn("FUSE drainInFlightOperations completed")
}
