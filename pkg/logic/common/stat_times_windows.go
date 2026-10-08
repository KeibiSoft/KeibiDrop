// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build windows

package common

import (
	"os"
	"syscall"
)

// statTimes returns access and birth time in unix nanoseconds for a stat
// result. Fallback when the platform data is unavailable: both equal mtime.
func statTimes(info os.FileInfo) (atimeNs, btimeNs uint64) {
	st, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		m := uint64(info.ModTime().UnixNano())
		return m, m
	}
	atimeNs = uint64(st.LastAccessTime.Nanoseconds())
	btimeNs = uint64(st.CreationTime.Nanoseconds())
	return atimeNs, btimeNs
}
