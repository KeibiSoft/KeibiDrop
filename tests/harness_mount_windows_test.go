// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build windows

package tests

import "os"

// isMountedDevStat: WinFsp creates the mount directory at mount time and
// removes it at unmount, so existence is the mounted signal.
func isMountedDevStat(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}
