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

// Larger values (10-16 MiB) can be faster but can also cap and regress.
// The best value depends on the hardware; measured copy speed varies widely.

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
	// Report the custom block size for better cp/dd I/O performance.
	// macOS cp uses fcopyfile, which sizes its buffer from the statfs block size.
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
	// Report the custom block size. cp sizes its read buffer from st_blksize.
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
	// Report the custom block size for better I/O performance.
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
// Do not add negative_vncache: it caches ENOENT results in the kernel vnode
// cache. When files arrive from a peer (git clone, file sync), Getattr
// returns ENOENT before the file exists. With negative_vncache, the kernel
// keeps returning ENOENT after the file appears. Git status then shows
// "deleted" files and ls misses files.
func getMountOptions(autoCache bool) []string {
	opts := []string{
		"-o", "volname=KeibiDrop",
		"-o", "local",
		"-o", "slow_statfs",
		"-o", "allow_other",
		"-o", "defer_permissions", // Defer permission checks to the FS (enables exec for git hooks).
		// noappledouble removed: Finder needs .DS_Store writes to succeed
		// for drag-and-drop. Peer sync filters .DS_Store out instead.
		"-o", "iosize=524288", // 512KB: matches ChunkSize, best throughput in benchmarks.
	}
	// auto_cache (opt-in via live_collab) flushes the page cache on open when
	// mtime/size changed, so a peer's same-size in-place edit shows live.
	// Without it, macFUSE drops its data cache only on a size change. The cost:
	// auto_cache discards dirty mmap pages before writeback and corrupts files
	// written via mmap (git's index). It is therefore off by default (git-safe).
	// The per-file KeepCache=false in OpenEx covers Linux without this hazard
	// but is a no-op on macFUSE, so the same-size case needs auto_cache here.
	if autoCache {
		opts = append(opts, "-o", "auto_cache")
	}
	return opts
}
