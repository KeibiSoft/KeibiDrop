// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build windows

package common

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// MountIsLive reports whether path is a WinFsp mount that is readable. A
// live mount has one of two shapes, measured on the Singapore box with the
// real daemon (2026-10-02): a drive letter is a volume root whose file
// system reports as FUSE; a directory mount point is a junction reparse
// point to the volume device. A plain directory is neither, so a mount
// directory created ahead of the attach, or left behind by a crash, never
// reads as ready. Listable is still the readiness signal, and an empty mount
// is live, hence the io.EOF case.
func MountIsLive(path string) bool {
	clean := filepath.Clean(path)
	if isVolumeRoot(clean) {
		// "X:" alone names the current directory of that drive; the root form does not.
		root := filepath.VolumeName(clean) + `\`
		return volumeIsFUSE(root) && listable(root)
	}
	return isReparsePoint(clean) && listable(clean)
}

// isVolumeRoot is true for "X:" and "X:\".
func isVolumeRoot(clean string) bool {
	vol := filepath.VolumeName(clean)
	return vol != "" && (clean == vol || clean == vol+`\`)
}

// volumeIsFUSE asks the volume for its file system name; WinFsp reports
// FUSE for a cgofuse volume, NTFS and the FAT family are the disks.
func volumeIsFUSE(clean string) bool {
	root := filepath.VolumeName(clean) + `\`
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	var fsName [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(rootPtr, nil, 0, nil, nil, nil, &fsName[0], uint32(len(fsName))); err != nil {
		return false
	}
	return strings.HasPrefix(strings.ToUpper(windows.UTF16ToString(fsName[:])), "FUSE")
}

// isReparsePoint is true for a junction or a symbolic link: the directory
// mount point WinFsp creates is a junction to \Device\Volume{...}.
func isReparsePoint(clean string) bool {
	fi, err := os.Lstat(clean)
	if err != nil {
		return false
	}
	data, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return false
	}
	return data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

func listable(clean string) bool {
	f, err := os.Open(clean)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}
