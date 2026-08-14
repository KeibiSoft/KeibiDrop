// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
	f, err := os.Open(path) // #nosec G304
	if err != nil {
		return nil, err
	}
	noUpdate := windows.Filetime{LowDateTime: 0xFFFFFFFF, HighDateTime: 0xFFFFFFFF}
	_ = windows.SetFileTime(windows.Handle(f.Fd()), nil, &noUpdate, nil)
	return f, nil
}
