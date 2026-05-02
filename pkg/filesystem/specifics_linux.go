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

//go:build linux

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
	dst.Bsize = uint64(src.Bsize)
	dst.Frsize = 1
	dst.Blocks = uint64(src.Blocks)
	dst.Bfree = uint64(src.Bfree)
	dst.Bavail = uint64(src.Bavail)
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
	dst.Blksize = int64(src.Blksize)
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
	dst.Blksize = src.Blksize
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
func getMountOptions() []string {
	return []string{"-o", "nonempty,allow_other"}
}
