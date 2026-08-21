// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build windows

package filesystem

import (
	"path/filepath"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

// st_blksize hint for Windows tools. 2 << 18 = 512 KiB.
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
	// IMPORTANT: always the optimized FilesystemBlockSize, never the source's
	// (often a remote peer's stat). Small read buffers crawl on FUSE. st_blocks
	// is in 512-byte units, derived from size. Keep it consistent with statFromFileInfo.
	dst.Blksize = int64(FilesystemBlockSize)
	dst.Blocks = (dst.Size + 511) / 512
}

// The autoCache (live_collab) flag is ignored on Windows on purpose: WinFsp
// manages its own cache coherence and invalidates on file-info change. A
// peer's same-size in-place edit shows without an auto_cache equivalent and
// without the mmap hazard. There is nothing to toggle here.
func getMountOptions(_ bool) []string {
	// uid=-1,gid=-1: WinFSP resolves ownership against the calling process's
	// token, not a fixed UID/GID. This is the Windows equivalent of macOS's
	// "allow_other,defer_permissions". Without it, WinFSP treats the current
	// user as "other" (mode 0755: no write), and every Create/Write call
	// returns ERROR_ACCESS_DENIED.
	return []string{"-o", "uid=-1,gid=-1"}
}
