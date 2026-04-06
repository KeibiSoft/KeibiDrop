// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build windows

package filesystem

import (
	"path/filepath"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// On Windows, `copy` works with 1 MiB file block size.
const FilesystemBlockSize = 2 << 18

func GetFreeDiskSpace(path string) (freeBytesAvail, totalNumberOfBytes, totalNumberFreeBytes uint64, err error) {
	err = windows.GetDiskFreeSpaceEx(windows.StringToUTF16Ptr(filepath.Clean(path)),
		&freeBytesAvail, &totalNumberOfBytes, &totalNumberFreeBytes)
	return
}

func setuidgid() func() {
	return func() {} // No-op on Windows.
}

func copyFusestatFromFusestat(dst *winfuse.Stat_t, src *winfuse.Stat_t) {
	if dst == nil || src == nil {
		return
	}
	*dst = *src
}

func getMountOptions() []string {
	return []string{} // WinFsp handles mount options via its own API.
}
