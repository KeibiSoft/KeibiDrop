// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build windows

package service

import (
	"os"

	"golang.org/x/sys/windows"
)

// openReadNoAtime opens path read-only and disables access-time updates on
// the handle (FILETIME -1 sentinel, per SetFileTime docs). Best effort: if
// the sentinel call fails the handle still serves reads.
func openReadNoAtime(path string) (*os.File, error) {
	f, err := openReadShared(path)
	if err != nil {
		return nil, err
	}
	noUpdate := windows.Filetime{LowDateTime: 0xFFFFFFFF, HighDateTime: 0xFFFFFFFF}
	_ = windows.SetFileTime(windows.Handle(f.Fd()), nil, &noUpdate, nil)
	return f, nil
}

// openReadShared opens path read-only to serve a peer, with delete sharing. A
// peer's read stream holds the handle until the peer closes the file, which
// on Windows comes late: without FILE_SHARE_DELETE the owner's own delete or
// rename of the file failed with "being used by another process"
// (2026-10-11). With it the name goes at once and the stream reads on.
func openReadShared(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
