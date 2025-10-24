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
	cleanMountPoint := filepath.Clean(mountPoint)
	pt := strings.Split(cleanMountPoint, "/")
	if len(pt) < 1 {
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

	// opts := []string{"volname=KeibiDrop", "local"}
	fs.host.Mount(cleanMountPoint, nil)
}

func (fs *FS) Unmount() {
	if fs.host == nil {
		return
	}

	// TODO: Also call umount on the MountPath in case its stuck or something.

	fs.host.Unmount()
	fs.Root = nil
}
