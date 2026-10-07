// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package service

import (
	"os"
	"strings"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
)

// internalPathMarkers name OS and KeibiDrop internal files that must never sync.
var internalPathMarkers = []string{
	".fuse_hidden",
	".fseventsd",
	".fseventuuid",
	".DS_Store",
	".kdbitmap",
}

// IsInternalPath reports whether the path must never sync: FUSE hidden renames,
// OS metadata, download bitmap sidecars, a file manager's trash folder.
func IsInternalPath(path string) bool {
	if types.IsOSTrashPath(path) {
		return true
	}
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
