// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build !windows

package common

import (
	"os"
	"path/filepath"
	"syscall"
)

// MountIsLive reports whether path is a mount point, by comparing its device
// id with its parent's. A directory that merely exists is not enough: the
// mount point is created before the filesystem is attached, so stat alone
// would report ready while reads still fail.
func MountIsLive(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	pst, pok := parent.Sys().(*syscall.Stat_t)
	if !ok || !pok {
		return false
	}
	return st.Dev != pst.Dev
}
