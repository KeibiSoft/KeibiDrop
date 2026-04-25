// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//go:build !windows && !android

package filesystem

import (
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

const platODIRECTORY = syscall.O_DIRECTORY
const platENODATA = syscall.ENODATA
const platDiskModeIsAuthoritative = true

func platTruncate(path string, size int64) error {
	return syscall.Truncate(path, size)
}

func platUnlink(path string) error {
	return syscall.Unlink(path)
}

func platOpen(path string, flags int, mode uint32) (int, error) {
	return syscall.Open(path, flags, mode)
}

func platClose(fd int) error {
	return syscall.Close(fd)
}

func platFsync(fd int) error {
	return syscall.Fsync(fd)
}

func platPread(fd int, buf []byte, offset int64) (int, error) {
	return syscall.Pread(fd, buf, offset)
}

func platPwrite(fd int, buf []byte, offset int64) (int, error) {
	return syscall.Pwrite(fd, buf, offset)
}

func platMkdir(path string, mode uint32) error {
	return syscall.Mkdir(path, mode)
}

func platMknod(path string, mode uint32, dev int) error {
	return syscall.Mknod(path, mode, dev)
}

func platChmod(path string, mode uint32) error {
	return syscall.Chmod(path, mode)
}

func platChown(path string, uid int, gid int) error {
	return syscall.Lchown(path, uid, gid)
}

// platLstat returns a winfuse.Stat_t populated via syscall.Lstat.
func platLstat(path string) (winfuse.Stat_t, error) {
	var raw syscall.Stat_t
	if err := syscall.Lstat(path, &raw); err != nil {
		return winfuse.Stat_t{}, err
	}
	var st winfuse.Stat_t
	copyFusestatFromGostat(&st, &raw)
	return st, nil
}

// platStat returns a winfuse.Stat_t populated via syscall.Stat.
func platStat(path string) (winfuse.Stat_t, error) {
	var raw syscall.Stat_t
	if err := syscall.Stat(path, &raw); err != nil {
		return winfuse.Stat_t{}, err
	}
	var st winfuse.Stat_t
	copyFusestatFromGostat(&st, &raw)
	return st, nil
}

// platStatfs returns a winfuse.Statfs_t populated via syscall.Statfs.
func platStatfs(path string) (winfuse.Statfs_t, error) {
	var raw syscall.Statfs_t
	if err := syscallStatfs(path, &raw); err != nil {
		return winfuse.Statfs_t{}, err
	}
	var st winfuse.Statfs_t
	copyFusestatfsFromGostatfs(&st, &raw)
	return st, nil
}
