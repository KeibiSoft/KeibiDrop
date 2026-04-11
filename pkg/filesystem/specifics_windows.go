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
	// uid=-1,gid=-1: instruct WinFSP to resolve ownership against the calling
	// process's token rather than a fixed UID/GID.  This is the Windows
	// equivalent of macOS's "allow_other,defer_permissions" — without it,
	// WinFSP treats the current user as "other" (mode 0755 → no write),
	// causing every Create/Write call to return ERROR_ACCESS_DENIED.
	return []string{"-o", "uid=-1,gid=-1"}
}
