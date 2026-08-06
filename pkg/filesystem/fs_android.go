// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build android

package filesystem

import (
	"log/slog"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

type Dir struct {
	LocalDownloadFolder string
}

type FS struct {
	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider
	PrefetchOnOpen     bool
	PrefetchAutoMB     int
	ReadAheadWindowMB  int // Unused on Android (no FUSE). Present so shared setup code compiles.
	PushOnWrite        bool
	AutoCache          bool // Unused on Android (no FUSE). Present so shared setup code compiles.
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
