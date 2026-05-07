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
	Root               *Dir
	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider
	PrefetchOnOpen     bool
	PushOnWrite        bool
}

func NewFS(_ *slog.Logger) *FS { return &FS{} }

func (fs *FS) Mount(_ string, _ bool, _ string) error { return nil }

func (fs *FS) Unmount() {}
