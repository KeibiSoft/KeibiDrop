// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build windows

package tests

import "os"

// isMountedDevStat: WinFsp creates the mount directory at mount time and
// removes it at unmount, so existence is the mounted signal.
func isMountedDevStat(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}
