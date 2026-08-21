// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build android

package filesystem

import (
	"log/slog"
	"os"
	"sync"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// File is a minimal Android stand-in so Dir.RemoteFiles below can satisfy
// BackfillRemoteFilesIntoFS's map type. Never populated (see RemoteFiles).
type File struct{}

type Dir struct {
	LocalDownloadFolder string

	// RemoteFilesLock and RemoteFiles back BackfillRemoteFilesIntoFS. Every
	// call site guards on Root() != nil first, and Root() is always nil
	// here, so neither field is ever actually touched.
	RemoteFilesLock sync.RWMutex
	RemoteFiles     map[string]*File
}

type FS struct {
	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider
	PrefetchOnOpen     bool
	PrefetchAutoMB     int
	ReadAheadWindowMB  int // Unused on Android (no FUSE). Present so shared setup code compiles.
	PushOnWrite        bool
	AutoCache          bool // Unused on Android (no FUSE). Present so shared setup code compiles.
	MountReadOnly      bool // Unused on Android (no FUSE mount to enforce EROFS). Present so shared setup code compiles.
	PreserveMetadata   bool // Unused on Android (no FUSE writer path). Present so shared setup code compiles.
}

func NewFS(_ *slog.Logger) *FS { return &FS{} }

// Root always returns nil on Android: no FUSE tree exists. Present so shared
// callers compile.
func (fs *FS) Root() *Dir { return nil }

func (fs *FS) Mount(_ string, _ bool, _ string) error { return nil }

func (fs *FS) Unmount() {}

// RefreshCallbacks is a no-op on Android: there is no FUSE Root to re-wire.
func (fs *FS) RefreshCallbacks() {}

// IsMounted always returns false on Android: no FUSE host is ever mounted.
func (fs *FS) IsMounted() bool { return false }

// ClearFiles is a no-op on Android: there are no FUSE file/dir maps to clear.
func (fs *FS) ClearFiles() {}

// CancelInFlight is a no-op on Android: no FUSE prefetch goroutines or
// stream pools exist to cancel.
func (fs *FS) CancelInFlight() {}

// EnsurePeerScope is a no-op on Android: there is no FUSE file cache to
// scope to a peer.
func (fs *FS) EnsurePeerScope(_ string) {}

// OpenShared behaves like plain os.OpenFile on Android: the Windows
// delete-sharing concern it guards against does not apply here.
func OpenShared(path string, flags int, mode uint32) (*os.File, error) {
	return os.OpenFile(path, flags, os.FileMode(mode))
}

// PendingAnnounceSuperseded always returns false on Android: every call site
// guards on Root() != nil first, and Root() is always nil here.
func (d *Dir) PendingAnnounceSuperseded(_ string) bool { return false }

// AddRemoteFileWithBase is a no-op on Android: its only caller,
// BackfillRemoteFilesIntoFS, guards on Root() != nil first, and Root() is
// always nil here.
func (d *Dir) AddRemoteFileWithBase(_ *slog.Logger, _, _ string, _ *winfuse.Stat_t, _ int64) error {
	return nil
}

// ApplyOriginMetadata is a no-op on Android: preserve-metadata is a FUSE
// feature not wired into the mobile API, so there is nothing to apply.
func ApplyOriginMetadata(_ string, _ uint32, _, _ int64) error { return nil }
