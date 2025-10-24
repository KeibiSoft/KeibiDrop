// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package synctracker

import (
	"sync"
)

type File struct {
	Name string

	RelativePath   string // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string // The Path on the local system.

	LastEditTime uint64 // Use time.Now().UnixNano().
	CreatedTime  uint64

	Size uint64
}

type SyncTracker struct {
	LocalFilesMu sync.RWMutex
	LocalFiles   map[string]*File

	RemoteFilesMu sync.RWMutex
	RemoteFiles   map[string]*File
}

func NewSyncTracker() *SyncTracker {
	return &SyncTracker{
		LocalFiles:  make(map[string]*File),
		RemoteFiles: make(map[string]*File),
	}
}
