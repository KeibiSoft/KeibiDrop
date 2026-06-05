// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
//
// Portions of this file are derived from the cgofuse project,
// which is licensed under the MIT License.
// Copyright (c) 2018–2023, Bill Zissimopoulos and cgofuse contributors.
// See https://github.com/billziss-gh/cgofuse for details.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux && !android

package filesystem

import (
	"path/filepath"
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

// On linux cp (1) works with 126 KiB file block size.
// Setting this value for the StatFS ensures optimal speed.

const FilesystemBlockSize = 2 << 17 // 256 KiB - optimal for Linux cp (uses 128 KiB blocks)

func GetFreeDiskSpace(path string) (freeBytesAvail, totalNumberOfBytes, totalNumberFreeBytes uint64, err error) {
	stat := unix.Statfs_t{}

	err = unix.Statfs(filepath.Clean(path), &stat)

	freeBytesAvail = stat.Bavail * uint64(stat.Bsize)
	totalNumberOfBytes = stat.Blocks * uint64(stat.Bsize)
	totalNumberFreeBytes = stat.Bfree * uint64(stat.Bsize)

	return
}

func copyFusestatfsFromGostatfs(dst *winfuse.Statfs_t, src *syscall.Statfs_t) {
	*dst = winfuse.Statfs_t{}
	// VERY IMPORTANT: report our optimized FilesystemBlockSize here too, and
	// rescale the block counts so total/free BYTES stay correct — statfs blocks
	// are in bsize units, so a larger bsize means proportionally fewer blocks
	// (blocks*bsize is invariant). Skipping the rescale would misreport df by 64x.
	srcBsize := uint64(src.Bsize)
	if srcBsize == 0 {
		srcBsize = 1
	}
	dst.Bsize = FilesystemBlockSize
	dst.Frsize = FilesystemBlockSize
	dst.Blocks = (uint64(src.Blocks) * srcBsize) / FilesystemBlockSize
	dst.Bfree = (uint64(src.Bfree) * srcBsize) / FilesystemBlockSize
	dst.Bavail = (uint64(src.Bavail) * srcBsize) / FilesystemBlockSize
	dst.Files = uint64(src.Files)
	dst.Ffree = uint64(src.Ffree)
	dst.Favail = uint64(src.Ffree)
	dst.Namemax = 255 // uint64(src.Namelen)
}

func copyFusestatFromGostat(dst *winfuse.Stat_t, src *syscall.Stat_t) {
	*dst = winfuse.Stat_t{}
	dst.Dev = uint64(src.Dev)
	dst.Ino = uint64(src.Ino)
	dst.Mode = uint32(src.Mode) // #nosec G115
	dst.Nlink = uint32(src.Nlink)
	dst.Uid = uint32(src.Uid)
	dst.Gid = uint32(src.Gid)
	dst.Rdev = uint64(src.Rdev)
	dst.Size = int64(src.Size)
	dst.Atim.Sec, dst.Atim.Nsec = int64(src.Atim.Sec), int64(src.Atim.Nsec)
	dst.Mtim.Sec, dst.Mtim.Nsec = int64(src.Mtim.Sec), int64(src.Mtim.Nsec)
	dst.Ctim.Sec, dst.Ctim.Nsec = int64(src.Ctim.Sec), int64(src.Ctim.Nsec)
	// VERY IMPORTANT: always report our optimized FilesystemBlockSize, never the
	// backing FS's st_blksize. cp/dd/rsync size their read buffer from it; a small
	// buffer is catastrophic on Linux FUSE (~33 vs ~232 MB/s).
	dst.Blksize = FilesystemBlockSize
	dst.Blocks = int64(src.Blocks)
}

func copyFusestatFromFusestat(dst *winfuse.Stat_t, src *winfuse.Stat_t) {
	*dst = winfuse.Stat_t{}
	dst.Dev = src.Dev
	dst.Ino = src.Ino
	dst.Mode = src.Mode
	dst.Nlink = src.Nlink
	dst.Uid = src.Uid
	dst.Gid = src.Gid
	dst.Rdev = src.Rdev
	dst.Size = src.Size
	dst.Atim.Sec, dst.Atim.Nsec = src.Atim.Sec, src.Atim.Nsec
	dst.Mtim.Sec, dst.Mtim.Nsec = src.Mtim.Sec, src.Mtim.Nsec
	dst.Ctim.Sec, dst.Ctim.Nsec = src.Ctim.Sec, src.Ctim.Nsec
	// VERY IMPORTANT: always our optimized FilesystemBlockSize, never the source's
	// (often a remote peer's stat). Small read buffers crawl on Linux FUSE.
	dst.Blksize = FilesystemBlockSize
	dst.Blocks = src.Blocks
	dst.Birthtim.Sec, dst.Birthtim.Nsec = src.Birthtim.Sec, src.Birthtim.Nsec
}

func syscallStatfs(path string, stat *syscall.Statfs_t) error {
	return syscall.Statfs(path, stat)
}

// getMountOptions returns Linux-specific FUSE mount options.
// nonempty: allow mounting on directories that already contain files.
// allow_other: let other users (e.g. postgres, mysql) access the mount.
//
//	Requires user_allow_other in /etc/fuse.conf.
//
// Note: NOT using default_permissions — it makes the kernel enforce POSIX
// chown rules (only root can chown), which blocks database init flows where
// MySQL/PostgreSQL need to chown their data directories.
//
// The autoCache (live_collab) flag is intentionally ignored on Linux: libfuse
// invalidates the page cache on open when FOPEN_KEEP_CACHE is absent (which
// OpenEx sets for remote files via KeepCache=false), so a peer's same-size
// in-place edit is already seen live WITHOUT auto_cache — and without the
// auto_cache mmap-writeback hazard that corrupts git on macOS. Linux gets both
// live collab and mmap safety for free, so there is nothing to toggle here.
func getMountOptions(_ bool) []string {
	return []string{"-o", "nonempty,allow_other,max_read=131072"}
}
