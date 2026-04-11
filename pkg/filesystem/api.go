// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

package filesystem

import (
	"log/slog"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

type FS struct {
	logger *slog.Logger

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider
	OnUnmountError     func(msg string) // Called when unmount fails (mount busy).

	// Collab sync options (set from env before Mount).
	PrefetchOnOpen bool // If true, fetch entire file on Open() and write to local disk.
	PushOnWrite    bool // If true, async push deltas to peer on Write().

	// Host.
	host      *winfuse.FileSystemHost
	Root      *Dir
	MountPath string // Set during Mount, used for cleanup verification.
}

func NewFS(logger *slog.Logger) *FS {
	return &FS{
		logger: logger,
	}
}

func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) {
	fs.logger.Warn("FUSE Mount starting",
		"mountPoint", mountPoint,
		"downloadPath", downloadPath,
		"isSecond", isSecond)

	cleanMountPoint := filepath.Clean(mountPoint)
	fs.MountPath = cleanMountPoint
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
		return
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
		OpenFileHandlers: make(map[uint64]*File),

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
		return
	}
	fs.logger.Warn("FUSE Mount completed", "mountPoint", cleanMountPoint)
}

func (fs *FS) Unmount() {
	fs.logger.Warn("FUSE Unmount starting", "hostNil", fs.host == nil)
	if fs.host == nil {
		fs.logger.Warn("FUSE Unmount skipped - host is nil")
		return
	}

	fs.host.Unmount()
	fs.Root = nil

	// Verify mount is actually gone. If a process holds a reference (e.g. terminal
	// cd'd into the mount), Unmount() succeeds but the mount stays busy.
	if fs.MountPath != "" {
		if fs.isMountBusy() {
			fs.logger.Error("FUSE Unmount: mount point still busy, trying force unmount", "path", fs.MountPath)
			fs.forceUnmount()
			if fs.isMountBusy() {
				fs.logger.Error("FUSE Unmount: force unmount failed — mount still busy. Close any terminals in the mount directory.", "path", fs.MountPath)
				if fs.OnUnmountError != nil {
					fs.OnUnmountError("Mount point busy: close any terminals or apps using " + fs.MountPath)
				}
			}
		}
	}

	fs.logger.Warn("FUSE Unmount completed")
}

// isMountBusy checks if the mount point is still a live FUSE mount.
func (fs *FS) isMountBusy() bool {
	// Try to stat the mount point. If it's still a FUSE mount and busy,
	// os.Stat will either hang or return "device not configured".
	// Use a simple check: see if /sbin/mount lists it.
	if runtime.GOOS == "windows" {
		return false // WinFSP handles cleanup differently.
	}
	out, err := exec.Command("mount").Output()
	if err != nil {
		return false
	}
	// macOS shows /private/var for /var mounts, check both.
	return strings.Contains(string(out), fs.MountPath) ||
		strings.Contains(string(out), "/private"+fs.MountPath)
}

// forceUnmount tries platform-specific force unmount.
func (fs *FS) forceUnmount() {
	switch runtime.GOOS {
	case "darwin":
		_ = exec.Command("/sbin/umount", "-f", fs.MountPath).Run()
	case "linux":
		_ = exec.Command("fusermount", "-u", fs.MountPath).Run()
	}
}
