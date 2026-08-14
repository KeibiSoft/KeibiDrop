// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package common

import (
	"os"
	"syscall"
)

// statTimes returns access and birth time in unix nanoseconds for a stat
// result. Linux syscall.Stat_t has no birth time; it falls back to mtime.
func statTimes(info os.FileInfo) (atimeNs, btimeNs uint64) {
	m := uint64(info.ModTime().UnixNano())
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return m, m
	}
	atimeNs = uint64(st.Atim.Sec)*1e9 + uint64(st.Atim.Nsec)
	return atimeNs, m
}
