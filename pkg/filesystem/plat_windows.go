// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//go:build windows

package filesystem

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	winfuse "github.com/winfsp/cgofuse/fuse"
	"golang.org/x/sys/windows"
)

const platENODATA = syscall.ENOENT // ENODATA does not exist on Windows. Map to ENOENT.
const platDiskModeIsAuthoritative = false

func platTruncate(path string, size int64) error {
	return os.Truncate(path, size)
}

func platUnlink(path string) error {
	return os.Remove(path)
}

// platRename replaces the target with POSIX semantics. MoveFileEx (os.Rename)
// cannot replace a file that ANY handle has open, even with delete sharing —
// and the long-lived CacheFD keeps the target open during downloads.
// FileRenameInfoEx + POSIX_SEMANTICS can (Win10+, NTFS); older systems fall
// back to os.Rename.
func platRename(oldpath, newpath string) error {
	werr := tryPosixRename(oldpath, newpath)
	if werr == nil {
		return nil
	}
	if rerr := os.Rename(oldpath, newpath); rerr == nil {
		return nil
	}
	// Last resort for the CACHE layer: some handle without delete sharing
	// pins the involved file (Go's own opens pass none). Copy the bytes to
	// the new name and retire the old name; identity above the cache lives
	// in the maps, so a briefly-lingering old cache file is invisible and
	// harmless, while failing the rename breaks the app's save.
	srcInfo, statErr := os.Stat(oldpath)
	in, ierr := platOpen(oldpath, syscall.O_RDONLY, 0)
	if ierr != nil {
		return fmt.Errorf("%w (posix-semantics rename)", werr)
	}
	out, oerr := platOpen(newpath, syscall.O_CREAT|syscall.O_TRUNC|syscall.O_WRONLY, 0644)
	if oerr != nil {
		_ = platClose(in)
		return fmt.Errorf("%w (posix-semantics rename; copy-open: %v)", werr, oerr)
	}
	bufc := make([]byte, 1<<20)
	var off int64
	for {
		n, rerr2 := platPread(in, bufc, off)
		if n > 0 {
			if _, werr2 := platPwrite(out, bufc[:n], off); werr2 != nil {
				_ = platClose(in)
				_ = platClose(out)
				return fmt.Errorf("%w (copy-fallback write: %v)", werr, werr2)
			}
			off += int64(n)
		}
		if rerr2 != nil || n == 0 {
			break
		}
	}
	_ = platClose(in)
	_ = platClose(out)
	// A rename PRESERVES the mtime; the copy above stamped a fresh one. A
	// fresh stamp raises the swapped file's identity above a concurrent
	// writer's and turns the conflict crossing into a mutual reject.
	if statErr == nil {
		_ = os.Chtimes(newpath, srcInfo.ModTime(), srcInfo.ModTime())
	}
	go func() {
		for i := 0; i < 40; i++ {
			if os.Remove(oldpath) == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()
	return nil
}

// tryPosixRename renames via FileRenameInfoEx + POSIX_SEMANTICS (Win10+,
// NTFS), which replaces open-with-delete-sharing targets that MoveFileEx
// refuses. Transient non-sharing holders (Go's own opens) get a bounded retry.
func tryPosixRename(oldpath, newpath string) error {
	const (
		fileRenameFlagReplaceIfExists = 0x1
		fileRenameFlagPosixSemantics  = 0x2
	)
	op, err := syscall.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	name, err := windows.UTF16FromString(`\??\` + newpath)
	if err != nil {
		return err
	}
	nameBytes := (len(name) - 1) * 2 // exclude the terminating NUL
	// FILE_RENAME_INFO layout (64-bit): Flags(4) pad(4) RootDirectory(8)
	// FileNameLength(4) FileName[]. Build the variable-size buffer by hand.
	buf := make([]byte, 20+nameBytes+2)
	binary.LittleEndian.PutUint32(buf[0:], fileRenameFlagReplaceIfExists|fileRenameFlagPosixSemantics)
	binary.LittleEndian.PutUint64(buf[8:], 0) // RootDirectory
	binary.LittleEndian.PutUint32(buf[16:], uint32(nameBytes))
	for i, u := range name[:len(name)-1] {
		binary.LittleEndian.PutUint16(buf[20+i*2:], u)
	}
	var werr error
	for i := 0; i < 40; i++ {
		h, herr := syscall.CreateFile(op, windows.DELETE,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if herr != nil {
			// The SOURCE itself can be pinned without delete sharing; that
			// also resolves when the transient holder closes.
			werr = herr
		} else {
			werr = windows.SetFileInformationByHandle(windows.Handle(h), windows.FileRenameInfoEx, &buf[0], uint32(len(buf)))
			_ = syscall.CloseHandle(h)
			if werr == nil {
				return nil
			}
		}
		if !errors.Is(werr, windows.ERROR_SHARING_VIOLATION) && !errors.Is(werr, windows.ERROR_ACCESS_DENIED) {
			return werr
		}
		if i == 0 && fuseOpLog {
			// Async: names this process's handles on the contended file.
			dumpHandlesFor(filepath.Base(oldpath))
			dumpHandlesFor(filepath.Base(newpath))
		}
		time.Sleep(25 * time.Millisecond)
	}
	return werr
}

func platOpen(path string, flags int, mode uint32) (int, error) {
	// Use CreateFile directly to pass FILE_SHARE_DELETE. Without it, no
	// other process (or this process's Rename/Unlink) can rename or delete
	// the file while the handle is open. That breaks POSIX semantics that
	// git and compilers rely on.
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

// platOpendir opens a directory handle on Windows. syscall.Open cannot open
// directories, so this uses CreateFile with FILE_FLAG_BACKUP_SEMANTICS.
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
		syscall.FILE_FLAG_BACKUP_SEMANTICS, // Required to open directories.
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
		// ERROR_HANDLE_EOF is normal. POSIX semantics: return 0 bytes, no error.
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

// platChown is a no-op on Windows: NTFS has no POSIX uid/gid mapping, so
// chown is tracked only in the in-memory stat. A caller that observes the
// mount via FUSE sees the requested uid/gid. Other Windows tooling does not.
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
