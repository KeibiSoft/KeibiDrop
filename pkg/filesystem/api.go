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
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

type FS struct {
	logger *slog.Logger

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	// Collab sync options (set from env before Mount).
	PrefetchOnOpen    bool // If true, Open() fetches the whole file and writes it to local disk.
	PrefetchAutoMB    int  // Files at or above this many MB prefetch on open (0=off; PrefetchOnOpen forces any size).
	ReadAheadWindowMB int  // MB cap for predictive sequential read-ahead (0=off). Each Dir converts it to blocks.
	PushOnWrite       bool // If true, Write() pushes deltas to the peer asynchronously.
	AutoCache         bool // If true (live_collab), add macFUSE auto_cache so a peer's same-size in-place edit shows live. Costs mmap-write integrity (git) on macOS; no-op on Linux/Windows.
	MountReadOnly     bool // If true, every mutating FUSE op returns EROFS. Peer updates still apply.

	// host and root are published by Mount and cleared by Unmount while other
	// goroutines (Run, teardown, gRPC handlers) read them, so access is atomic.
	host atomic.Pointer[winfuse.FileSystemHost]
	root atomic.Pointer[Dir]

	// ctxMu guards ctx/cancel. CancelInFlight and ClearFiles rebuild the pair
	// from RPC and teardown goroutines while Mount and Unmount read it. Never
	// hold ctxMu across a blocking call.
	ctxMu  sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc

	mountPoint string

	// cacheOwnerFP is the actual fingerprint of the peer whose files are
	// cached/shown. A connection to a different peer drops the cache first, so
	// one peer's artifacts never leak to the next. The same peer on reconnect
	// or resume keeps the cache: no re-fetch, files still there.
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

// Root returns the mounted root dir, or nil while unmounted.
func (fs *FS) Root() *Dir {
	return fs.root.Load()
}

// SetRoot publishes the root dir. Mount sets it in production; tests use it
// to wire a hand-built tree.
func (fs *FS) SetRoot(root *Dir) {
	fs.root.Store(root)
}

// Mount blocks for the lifetime of the FUSE session. It returns an error at
// once if the mount point is invalid or host.Mount() fails. On success it
// returns nil only after a clean unmount.
func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) error {
	fs.logger.Warn("FUSE Mount starting",
		"mountPoint", mountPoint,
		"downloadPath", downloadPath,
		"isSecond", isSecond)

	cleanMountPoint := filepath.Clean(mountPoint)

	// Clean up stale FUSE mounts from a previous crash.
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(cleanMountPoint); err == nil {
			// Read the directory as a probe. A "device not configured" failure
			// means a stale FUSE mount that needs a force-unmount.
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

	// On Windows, normalize drive-letter mount points for WinFSP:
	//   "K:."  (filepath.Clean("K:"))  -> "K:"
	//   "K:\"  (filepath.Clean("K:\")) -> "K:"
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

	// WinFSP rejects an existing mount path. Remove a stale empty dir left by a
	// crash or an older build. os.Remove only deletes an empty dir, so no data is lost.
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

		LocalDownloadFolder: filepath.Clean(downloadPath),

		OpenMapLock:      sync.RWMutex{},
		OpenFileHandlers: make(map[uint64]*HandleEntry),

		Adm:       sync.RWMutex{},
		AllDirMap: make(map[string]*Dir),

		AfmLock:    sync.RWMutex{},
		AllFileMap: make(map[string]*File),

		PrefetchOnOpen:        fs.PrefetchOnOpen,
		PrefetchAutoMB:        fs.PrefetchAutoMB,
		ReadAheadWindowBlocks: readAheadWindowBlocks(fs.ReadAheadWindowMB),
		PushOnWrite:           fs.PushOnWrite,
		MountReadOnly:         fs.MountReadOnly,

		RemoteFilesLock: sync.RWMutex{},
		RemoteFiles:     make(map[string]*File),

		PrefetchSem: make(chan struct{}, 8), // Maximum 8 concurrent prefetches.
	}

	root.Root = root
	root.SetCallbacks(fs.OnLocalChange, fs.OpenStreamProvider)
	fs.ctxMu.Lock()
	ctx := fs.ctx
	fs.ctxMu.Unlock()
	root.SetCtx(ctx)
	fs.root.Store(root)

	host := winfuse.NewFileSystemHost(root)
	host.SetCapReaddirPlus(true)
	host.SetUseIno(true)
	fs.host.Store(host)
	fs.mountPoint = cleanMountPoint

	opts := getMountOptions(fs.AutoCache)

	fs.logger.Warn("FUSE Mount calling host.Mount", "cleanMountPoint", cleanMountPoint, "opts", opts)
	ok := host.Mount(cleanMountPoint, opts)
	if !ok {
		// Reset host/Root so IsMounted() reports false. Otherwise a failed mount
		// leaves them set. The next reconnect then takes the "already mounted,
		// waiting" branch, never retries, and hangs the mount permanently.
		fs.host.Store(nil)
		fs.root.Store(nil)
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

// isWindowsDirMountPoint reports whether p is a Windows directory mount path, not a drive ("K:").
func isWindowsDirMountPoint(goos, p string) bool {
	return goos == "windows" && (len(p) != 2 || p[1] != ':')
}

func (fs *FS) Unmount() {
	host := fs.host.Load()
	fs.logger.Warn("FUSE Unmount starting", "hostNil", host == nil)
	if host == nil {
		fs.logger.Warn("FUSE Unmount skipped - host is nil")
		return
	}

	fs.ctxMu.Lock()
	cancel := fs.cancel
	fs.ctxMu.Unlock()
	cancel()

	if fs.root.Load() != nil {
		fs.drainInFlightOperations()
	}

	done := make(chan struct{})
	go func() {
		host.Unmount()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		fs.logger.Warn("FUSE Unmount timed out, force-unmounting", "mountPoint", fs.mountPoint)
		fs.forceUnmount()
		<-done
	}
	fs.root.Store(nil)
	fs.logger.Warn("FUSE Unmount completed")
}

// RefreshCallbacks updates the Root's callbacks to match the FS-level ones.
// Call it after setupFilesystem re-wires OnLocalChange/OpenStreamProvider,
// so the persistent Root uses the new session's gRPC client.
func (fs *FS) RefreshCallbacks() {
	root := fs.root.Load()
	if root == nil {
		return
	}
	root.SetCallbacks(fs.OnLocalChange, fs.OpenStreamProvider)
	fs.ctxMu.Lock()
	ctx := fs.ctx
	fs.ctxMu.Unlock()
	root.SetCtx(ctx)
}

// IsMounted reports whether the FUSE host is active.
func (fs *FS) IsMounted() bool {
	return fs.host.Load() != nil && fs.root.Load() != nil
}

// ClearFiles cancels in-flight operations and clears all file/dir maps.
// The mount stays alive as an empty folder, so a reconnect after disconnect
// reuses it (cgofuse allows only one mount per process).
func (fs *FS) ClearFiles() {
	fs.ctxMu.Lock()
	cancel := fs.cancel
	fs.ctxMu.Unlock()
	cancel()
	root := fs.root.Load()
	if root != nil {
		fs.drainInFlightOperations()
		root.AfmLock.Lock()
		root.AllFileMap = make(map[string]*File)
		root.AfmLock.Unlock()
		root.Adm.Lock()
		root.AllDirMap = make(map[string]*Dir)
		root.Adm.Unlock()
		root.RemoteFilesLock.Lock()
		root.RemoteFiles = make(map[string]*File)
		root.RemoteFilesLock.Unlock()
		root.OpenMapLock.Lock()
		root.OpenFileHandlers = make(map[uint64]*HandleEntry)
		root.OpenMapLock.Unlock()
	}
	// Fresh context for the next session.
	fs.ctxMu.Lock()
	fs.ctx, fs.cancel = context.WithCancel(context.Background())
	ctx := fs.ctx
	fs.ctxMu.Unlock()
	if root != nil {
		root.SetCtx(ctx)
	}
}

// CancelInFlight cancels in-flight reads/prefetches and resets the FUSE
// context. It preserves the cached file view (maps + bitmaps). Use it on a
// disconnect from a peer that may reconnect or resume: the same peer finds
// its files still there and resumes on-demand with no re-fetch. The full
// ClearFiles wipe is only for a switch to a different peer (see EnsurePeerScope).
func (fs *FS) CancelInFlight() {
	fs.ctxMu.Lock()
	cancel := fs.cancel
	fs.ctxMu.Unlock()
	cancel()
	root := fs.root.Load()
	if root != nil {
		fs.drainInFlightOperations()
	}
	// Fresh context so the next session's reads do not start cancelled.
	fs.ctxMu.Lock()
	fs.ctx, fs.cancel = context.WithCancel(context.Background())
	ctx := fs.ctx
	fs.ctxMu.Unlock()
	if root != nil {
		root.SetCtx(ctx)
	}
}

// EnsurePeerScope binds the cached file view to a peer identity. A different
// peer than the cached one drops the cache first, so one peer's artifacts
// never leak to the next. The same peer (reconnect or later session) keeps
// the cache with no re-fetch. The first peer adopts the empty cache without
// a clear. Repeat calls are idempotent and cheap. fp is the peer's actual
// verified fingerprint, so this is correct in TOFU mode too.
func (fs *FS) EnsurePeerScope(fp string) {
	if fp == "" {
		return // Unknown identity: never risk a spurious wipe.
	}
	fs.cacheOwnerMu.Lock()
	defer fs.cacheOwnerMu.Unlock()
	if fs.cacheOwnerFP == fp {
		return // Same peer: keep the cache.
	}
	if fs.cacheOwnerFP != "" {
		fs.ClearFiles() // Different peer: drop the previous peer's view.
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
		// WinFsp cleans up via host.Unmount(). No force path is needed.
	}
}

// drainInFlightOperations cancels all prefetch goroutines and closes all
// stream pools on open file handles. Pending FUSE Read/Open handlers then
// unblock at once instead of waiting for gRPC timeouts.
func (fs *FS) drainInFlightOperations() {
	root := fs.root.Load()
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

	// 2. Close all stream pools on open file handles. An in-flight
	//    pool.ReadAt() call inside a FUSE Read handler then fails at once
	//    instead of blocking on the 10s timeout.
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

// OpenShared opens path with FILE_SHARE_DELETE on Windows so a long-lived
// writer handle does not block rename or unlink of the file (plain os.OpenFile
// passes no delete sharing). Unix behaves like os.OpenFile.
func OpenShared(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := platOpen(path, flags, mode)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// PendingAnnounceSuperseded reports that path's queued ADD announce describes
// bytes whose authority was lost to an acceptance: no open edit session and
// LocalNewer cleared. The CancelPendingNotify fast path usually wins the race;
// this state check closes it when the flush fires first (CI-speed runners
// bounced the peer's own bytes back as a newer edit and diverged).
func (d *Dir) PendingAnnounceSuperseded(path string) bool {
	d.AfmLock.RLock()
	f := d.AllFileMap[path]
	d.AfmLock.RUnlock()
	if f == nil {
		return false
	}
	f.metaMu.RLock()
	defer f.metaMu.RUnlock()
	return !f.LocalNewer && !f.NotRemoteSynced
}

// RenameShared renames with the platform strategy platRename implements:
// POSIX-semantics replace plus copy fallback on Windows, plain rename on
// unix. Receive-path disk moves need it for the same reason the FUSE Rename
// does — pinned cache handles.
func RenameShared(oldpath, newpath string) error {
	return platRename(oldpath, newpath)
}
