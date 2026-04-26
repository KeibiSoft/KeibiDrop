// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

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
					exec.Command("diskutil", "unmount", "force", cleanMountPoint).Run()
				} else {
					exec.Command("fusermount", "-u", cleanMountPoint).Run()
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

	// TODO: Also call umount on the MountPath in case its stuck or something.

	fs.host.Unmount()
	fs.Root = nil
	fs.logger.Warn("FUSE Unmount completed")
}
