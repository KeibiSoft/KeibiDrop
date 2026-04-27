// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//go:build windows

package filesystem

import (
	"os"
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

const platODIRECTORY = 0           // O_DIRECTORY not available on Windows.
const platENODATA = syscall.ENOENT // ENODATA does not exist on Windows; map to ENOENT.
const platDiskModeIsAuthoritative = false

func platTruncate(path string, size int64) error {
	return os.Truncate(path, size)
}

func platUnlink(path string) error {
	return os.Remove(path)
}

func platOpen(path string, flags int, mode uint32) (int, error) {
	// Use CreateFile directly so we can pass FILE_SHARE_DELETE.
	// Without this, no other process (or our own Rename/Unlink) can
	// rename or delete the file while our handle is open — breaking
	// POSIX semantics that git, compilers, etc. rely on.
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}

	access := uint32(syscall.GENERIC_READ)
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		access = syscall.GENERIC_READ | syscall.GENERIC_WRITE
	}

	shareMode := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE)

	var disposition uint32
	switch {
	case flags&(syscall.O_CREAT|syscall.O_EXCL) == (syscall.O_CREAT | syscall.O_EXCL):
		disposition = syscall.CREATE_NEW
	case flags&(syscall.O_CREAT|syscall.O_TRUNC) == (syscall.O_CREAT | syscall.O_TRUNC):
		disposition = syscall.CREATE_ALWAYS
	case flags&syscall.O_CREAT != 0:
		disposition = syscall.OPEN_ALWAYS
	case flags&syscall.O_TRUNC != 0:
		disposition = syscall.TRUNCATE_EXISTING
	default:
		disposition = syscall.OPEN_EXISTING
	}

	attrs := uint32(syscall.FILE_ATTRIBUTE_NORMAL)

	h, err := syscall.CreateFile(p, access, shareMode, nil, disposition, attrs, 0)
	if err != nil {
		return 0, err
	}
	return int(h), nil
}

// platOpendir opens a directory handle on Windows. syscall.Open can't open
// directories — we need CreateFile with FILE_FLAG_BACKUP_SEMANTICS.
func platOpendir(path string) (int, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS, // Required to open directories
		0,
	)
	if err != nil {
		return 0, err
	}
	return int(h), nil
}

func platClose(fd int) error {
	return syscall.CloseHandle(syscall.Handle(fd))
}

func platFsync(fd int) error {
	return windows.FlushFileBuffers(windows.Handle(fd))
}

func platPread(fd int, buf []byte, offset int64) (int, error) {
	h := windows.Handle(fd)
	var overlapped windows.Overlapped
	overlapped.Offset = uint32(offset)
	overlapped.OffsetHigh = uint32(offset >> 32)
	var done uint32
	err := windows.ReadFile(h, buf, &done, &overlapped)
	if err != nil {
		// ERROR_HANDLE_EOF is normal — POSIX semantics: return 0 bytes, no error.
		if err == windows.ERROR_HANDLE_EOF {
			return int(done), nil
		}
		return int(done), err
	}
	return int(done), nil
}

func platPwrite(fd int, buf []byte, offset int64) (int, error) {
	h := windows.Handle(fd)
	var overlapped windows.Overlapped
	overlapped.Offset = uint32(offset)
	overlapped.OffsetHigh = uint32(offset >> 32)
	var done uint32
	err := windows.WriteFile(h, buf, &done, &overlapped)
	if err != nil {
		return int(done), err
	}
	return int(done), nil
}

func platMkdir(path string, mode uint32) error {
	return os.Mkdir(path, os.FileMode(mode))
}

func platMknod(path string, mode uint32, dev int) error {
	// mknod does not exist on Windows.
	return syscall.ENOSYS
}

func platChmod(path string, mode uint32) error {
	return os.Chmod(path, os.FileMode(mode&0o777))
}

// platChown is a no-op on Windows: NTFS has no POSIX uid/gid mapping, so chown
// is only tracked in the in-memory stat. A caller observing the mount via FUSE
// will see the requested uid/gid; other Windows tooling will not.
func platChown(path string, uid int, gid int) error {
	return nil
}

// platLstat returns a winfuse.Stat_t populated via os.Lstat.
func platLstat(path string) (winfuse.Stat_t, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return winfuse.Stat_t{}, err
	}
	return statFromFileInfo(info), nil
}

// platStat returns a winfuse.Stat_t populated via os.Stat.
func platStat(path string) (winfuse.Stat_t, error) {
	info, err := os.Stat(path)
	if err != nil {
		return winfuse.Stat_t{}, err
	}
	return statFromFileInfo(info), nil
}

// platStatfs returns a winfuse.Statfs_t via GetDiskFreeSpaceEx.
func platStatfs(path string) (winfuse.Statfs_t, error) {
	var freeAvail, totalBytes, totalFree uint64
	err := windows.GetDiskFreeSpaceEx(
		windows.StringToUTF16Ptr(path),
		&freeAvail, &totalBytes, &totalFree,
	)
	if err != nil {
		return winfuse.Statfs_t{}, err
	}
	bs := uint64(FilesystemBlockSize)
	return winfuse.Statfs_t{
		Bsize:   bs,
		Frsize:  bs,
		Blocks:  totalBytes / bs,
		Bfree:   totalFree / bs,
		Bavail:  freeAvail / bs,
		Namemax: 255,
	}, nil
}

func statFromFileInfo(info os.FileInfo) winfuse.Stat_t {
	var st winfuse.Stat_t
	st.Size = info.Size()
	st.Mtim.Sec = info.ModTime().Unix()
	st.Mtim.Nsec = int64(info.ModTime().Nanosecond())
	st.Atim = st.Mtim
	st.Ctim = st.Mtim
	st.Birthtim = st.Mtim
	if info.IsDir() {
		st.Mode = winfuse.S_IFDIR | 0755
	} else {
		st.Mode = winfuse.S_IFREG | uint32(info.Mode().Perm())
	}
	st.Uid = ^uint32(0)
	st.Gid = ^uint32(0)
	st.Nlink = 1
	st.Blksize = int64(FilesystemBlockSize)
	st.Blocks = (st.Size + 511) / 512
	return st
}
