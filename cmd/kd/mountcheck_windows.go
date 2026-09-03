// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build windows

package main

import (
	"errors"
	"io"
	"os"
)

// isLiveMount reports whether the WinFsp mount is readable. Windows has no
// device-id comparison to make here: WinFsp owns the whole path (a drive
// letter, or a directory it creates itself), so becoming listable is the
// readiness signal. An empty mount is still a live mount, hence the io.EOF
// case.
func isLiveMount(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	return err == nil || errors.Is(err, io.EOF)
}
