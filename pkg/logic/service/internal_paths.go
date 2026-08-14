// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package service

import (
	"os"
	"strings"
)

// internalPathMarkers name OS and KeibiDrop internal files that must never sync.
var internalPathMarkers = []string{
	".fuse_hidden",
	".fseventsd",
	".fseventuuid",
	".DS_Store",
	".kdbitmap",
}

// IsInternalPath reports whether the path names an internal file that must
// never sync: FUSE hidden renames, macOS metadata, download bitmap sidecars.
func IsInternalPath(path string) bool {
	for _, m := range internalPathMarkers {
		if strings.Contains(path, m) {
			return true
		}
	}
	return false
}

// openServeRead opens a local file to serve peer reads. With share_read_only
// (forensic posture) the open does not update the file's access time where
// the platform allows: Linux O_NOATIME, Windows handle sentinel. macOS has
// no per-handle equivalent; there the source mount's noatime policy decides.
func (kd *KeibidropServiceImpl) openServeRead(path string) (*os.File, error) {
	if kd.ShareReadOnly {
		return openReadNoAtime(path)
	}
	return os.Open(path) // #nosec G304
}
