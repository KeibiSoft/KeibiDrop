// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build android

// Package filesystem provides FUSE virtual filesystem support.
// On Android, FUSE is unavailable. This file provides no-op stubs
// for the FS type so that pkg/logic/common compiles without cgofuse.
package filesystem

import (
	"log/slog"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// FS is a no-op stub on Android. FUSE is not supported on this platform.
type FS struct {
	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	PrefetchOnOpen bool
	PushOnWrite    bool
}

// NewFS returns a new FS stub. FUSE is unavailable on Android.
func NewFS(_ *slog.Logger) *FS {
	return &FS{}
}

// Mount is a no-op on Android. The error return matches the desktop signature.
func (fs *FS) Mount(_ string, _ bool, _ string) error { return nil }

// Unmount is a no-op on Android.
func (fs *FS) Unmount() {}
