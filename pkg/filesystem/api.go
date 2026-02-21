// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"log/slog"
	"path/filepath"
	"strings"
	"sync"

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

func (fs *FS) Mount(mountPoint string, isSecond bool, downloadPath string) {
	fs.logger.Warn("FUSE Mount starting",
		"mountPoint", mountPoint,
		"downloadPath", downloadPath,
		"isSecond", isSecond)

	cleanMountPoint := filepath.Clean(mountPoint)
	pt := strings.Split(cleanMountPoint, "/")
	if len(pt) < 1 {
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
	fs.host.Mount(cleanMountPoint, opts)
	fs.logger.Warn("FUSE Mount completed", "mountPoint", cleanMountPoint)
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
