// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package checkfuse

import (
	"os"
	"runtime"
)

func IsFUSEPresent() bool {
	switch runtime.GOOS {
	case "windows":
		return exists(`C:\Windows\System32\winfsp-x64.dll`)
	case "darwin":
		return exists(`/usr/local/lib/libfuse.dylib`) ||
			exists(`/Library/Filesystems/macfuse.fs`)
	case "linux":
		return exists(`/lib/x86_64-linux-gnu/libfuse.so.2`) ||
			exists(`/usr/lib/libfuse.so`) ||
			exists(`/usr/lib/x86_64-linux-gnu/libfuse3.so`)
	default:
		return false
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
