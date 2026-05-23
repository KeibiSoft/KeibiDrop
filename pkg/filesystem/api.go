//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
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
	PrefetchOnOpen bool // If true, fetch entire file on Open() and write to local disk.
	PushOnWrite    bool // If true, async push deltas to peer on Write().

	// Host.
	host *winfuse.FileSystemHost
	Root *Dir
}

func NewFS(logger *slog.Logger) *FS {
	return &FS{
		logger: logger,
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

		PrefetchOnOpen: fs.PrefetchOnOpen,
		PushOnWrite:    fs.PushOnWrite,

		RemoteFilesLock: sync.RWMutex{},
		RemoteFiles:     make(map[string]*File),

		PrefetchSem: make(chan struct{}, 8), // max 8 concurrent prefetches
	}

	root.Root = root
	fs.Root = root

	host := winfuse.NewFileSystemHost(root)

	// I think this is windows specific.
	host.SetCapReaddirPlus(true)
	// Fuse3 only.
	host.SetUseIno(true)

	fs.host = host

	opts := getMountOptions()

	fs.logger.Warn("FUSE Mount calling host.Mount", "cleanMountPoint", cleanMountPoint, "opts", opts)
	ok := fs.host.Mount(cleanMountPoint, opts)
	if !ok {
		fs.logger.Error("FUSE Mount failed", "mountPoint", cleanMountPoint)
		return fmt.Errorf("FUSE mount failed for %s: ensure user_allow_other is set in /etc/fuse.conf, or run with KD_NO_FUSE=1", cleanMountPoint)
	}
	fs.logger.Warn("FUSE Mount completed", "mountPoint", cleanMountPoint)
	return nil
}

func (fs *FS) Unmount() {
	fs.logger.Warn("FUSE Unmount starting", "hostNil", fs.host == nil)
	if fs.host == nil {
		fs.logger.Warn("FUSE Unmount skipped - host is nil")
		return
	}

	// Cancel all in-flight gRPC operations BEFORE asking the kernel to
	// unmount. The kernel waits for every pending FUSE operation (Read,
	// Getattr, etc.) to return before completing the unmount. If those
	// handlers are blocked on gRPC calls to a disconnected peer (up to
	// 10s timeout * 3 retries each), host.Unmount() hangs for 30+ seconds.
	//
	// By cancelling prefetch contexts and closing stream pools first, those
	// handlers hit context.Canceled immediately and return, allowing the
	// kernel to complete the unmount promptly.
	if fs.Root != nil {
		fs.drainInFlightOperations()
	}

	fs.host.Unmount()
	fs.Root = nil
	fs.logger.Warn("FUSE Unmount completed")
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
