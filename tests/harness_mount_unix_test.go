// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !windows

package tests

import (
	"os"
	"path/filepath"
	"syscall"
)

// isMountedDevStat reports a live mount by device id: the dir's device
// differs from its parent's. Touches only the two paths — a mount-table walk
// blocks on any wedged mount in the system.
func isMountedDevStat(dir string) bool {
	fi, err := os.Stat(dir)
	if err != nil {
		return false
	}
	pi, err := os.Stat(filepath.Dir(dir))
	if err != nil {
		return false
	}
	fs, ok1 := fi.Sys().(*syscall.Stat_t)
	ps, ok2 := pi.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false
	}
	return fs.Dev != ps.Dev
}
