// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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
