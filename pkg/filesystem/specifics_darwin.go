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

//go:build darwin

package filesystem

import (
	"path/filepath"
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/unix"
)

// My note is the following:
// You can increase it to 10MiB or 16MiB, and depending on the processor,
// it will be faster, but it might cap and downgrade at some point.
// I cannot provide a realistic best value as I have used different hardware specs
// for testing and finding this values, and at the end of the day it will be missleading
// to say: on intel i7 from 2018 Thinkpad 480T (windows + linux), I had 500 MB/s copy speed.
// But on Mac M3 had 1.2 GB/s sometimes up to 2GB/s
// And on Mac Intel I did not benchmark yet.

// FilesystemBlockSize is the optimal I/O block size for cp/dd on macOS (2 MiB).
const FilesystemBlockSize = 2 << 20

// GetFreeDiskSpace returns free, total, and available disk space for the given path.
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
	// Use our custom block size for better I/O performance with cp/dd
	// macOS cp uses fcopyfile which respects statfs block size for buffer sizing
	dst.Bsize = FilesystemBlockSize
	dst.Frsize = FilesystemBlockSize
	// Recalculate block counts: (original_blocks * original_bsize) / new_bsize
	srcBsize := uint64(src.Bsize)
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
	dst.Dev = uint64(src.Dev)     // #nosec G115 -- dev_t is always non-negative
	dst.Ino = uint64(src.Ino)     // #nosec G115 -- ino_t is always non-negative
	dst.Mode = uint32(src.Mode)   // #nosec G115 -- mode_t fits in uint32
	dst.Nlink = uint32(src.Nlink) // #nosec G115 -- nlink_t fits in uint32
	dst.Uid = uint32(src.Uid)     // #nosec G115 -- uid_t fits in uint32
	dst.Gid = uint32(src.Gid)     // #nosec G115 -- gid_t fits in uint32
	dst.Rdev = uint64(src.Rdev)   // #nosec G115 -- dev_t is always non-negative
	dst.Size = int64(src.Size)
	dst.Atim.Sec, dst.Atim.Nsec = src.Atimespec.Sec, src.Atimespec.Nsec
	dst.Mtim.Sec, dst.Mtim.Nsec = src.Mtimespec.Sec, src.Mtimespec.Nsec
	dst.Ctim.Sec, dst.Ctim.Nsec = src.Ctimespec.Sec, src.Ctimespec.Nsec
	// Use our custom block size for better I/O performance - cp uses st_blksize for buffer sizing
	dst.Blksize = FilesystemBlockSize
	dst.Blocks = int64(src.Blocks)
	dst.Birthtim.Sec, dst.Birthtim.Nsec = src.Birthtimespec.Sec, src.Birthtimespec.Nsec
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
	// Use our custom block size for better I/O performance
	dst.Blksize = FilesystemBlockSize
	dst.Blocks = src.Blocks
	dst.Birthtim.Sec, dst.Birthtim.Nsec = src.Birthtim.Sec, src.Birthtim.Nsec
}

func syscallStatfs(path string, stat *syscall.Statfs_t) error {
	return syscall.Statfs(path, stat)
}

// getMountOptions returns macOS-specific FUSE mount options.
// See: https://github.com/macfuse/macfuse/wiki/Mount-Options
//
// NOTE: Do NOT add negative_vncache — it caches ENOENT results in the kernel
// vnode cache. When files arrive from a peer (git clone, file sync), Getattr
// returns ENOENT before the file exists. With negative_vncache, the kernel
// keeps returning ENOENT even after the file appears, causing "deleted" files
// in git status and missing files in ls.
func getMountOptions() []string {
	return []string{
		"-o", "volname=KeibiDrop",
		"-o", "local",
		"-o", "slow_statfs",
		"-o", "allow_other",
		"-o", "defer_permissions", // Defer permission checks to the FS (enables exec for git hooks).
		// noappledouble removed: Finder needs .DS_Store writes to succeed
		// for drag-and-drop to work. We filter .DS_Store from peer sync instead.
		"-o", "iosize=524288", // 512KB — matches ChunkSize, best throughput in benchmarks.
	}
}
