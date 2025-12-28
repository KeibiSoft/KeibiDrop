// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"log/slog"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// The plan is like this:
// Mounted filesystem for Alice is visible at "mountedPath".
// When Alice adds files from her machine to the filesystem,
// *Blip blip blop*, a new File{} gets created which represents
// a "symlink" to the original file which Bob can now access.
// For folders it's a bit different, as it does not create a "symlink"
// in the sense that Bob can navigate outside of the mapped folder
// but in the sense that Dir{} is mapped to the underlying one
// and all the children of the underlying one are mapped inside our Dir{}.

// Note: I use a tree hierarchy, not the most efficient way when it comes to lookups.
// I might flatten it in the future.
// Root -> Dir -> Dir -> File
//    | -> File
//    | -> Dir -> File

type Dir struct {
	logger *slog.Logger

	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string `json:"pathOnLocalSystem"` // The Path on the local system.

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	LocalDownloadFolder string // The folder where the files from the peer are downloaded.

	Parent *Dir
	Root   *Dir

	OpenFileHandlers map[uint64]*File
	OpenMapLock      sync.RWMutex

	Adm       sync.RWMutex
	AllDirMap map[string]*Dir

	AfmLock    sync.RWMutex
	AllFileMap map[string]*File

	stat *winfuse.Stat_t

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	// Collab sync options (propagated from FS).
	PrefetchOnOpen bool // If true, fetch entire file on Open() and write to local disk.
	PushOnWrite    bool // If true, async push deltas to peer on Write().

	RemoteFilesLock sync.RWMutex
	RemoteFiles     map[string]*File
}

type File struct {
	logger *slog.Logger

	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath string `json:"relativePath"` // Relative (to root) path in the mounted filesystem.

	RealPathOfFile string // The Path on the local system.

	Parent *Dir
	Root   *Dir

	LastEditTime uint64 `json:"lastEdit"` // Use time.Now().UnixNano().
	CreatedTime  uint64 `json:"createdAt"`

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	NotLocalSynced  bool
	NotRemoteSynced bool

	LocalNewer bool

	HadEdits bool

	openFileCounter OpenFileCounter

	StreamProvider   types.FileStreamProvider
	RemoteFileStream types.RemoteFileStream
	StreamCancel     context.CancelFunc // Cancel function for the stream context

	OnLocalChange func(event types.FileEvent)

	stat *winfuse.Stat_t
}

func (f *File) NotifyPeer() {}

// Use it as a singleton only when setting up the filesystem.
// (In the mount command).
// I do not enforce it as a singleton, as my philospohy
// is to not have package global var, just a
// call chain of functions from the entrypoint of
// the program.

// Create and Open calls must have a corresponding Release call.
type OpenFileCounter struct {
	mu      *sync.Mutex
	counter uint64
}

func (ofc *OpenFileCounter) Open() {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	ofc.counter++
}

func (ofc *OpenFileCounter) Release() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	if ofc.counter == 0 {
		return 0
	}

	ofc.counter--
	return ofc.counter
}

func (ofc *OpenFileCounter) CountOpenDescriptors() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	return ofc.counter
}
