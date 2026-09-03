// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// isLiveMount reports whether path is a mount point, by comparing its device
// id with its parent's. A directory that merely exists is not enough: the
// mount point is created before the filesystem is attached, so stat alone
// would report ready while reads still fail.
func isLiveMount(path string) bool {
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
