// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package checkfuse

import (
	"log/slog"
	"os"
	"runtime"
)

func IsFUSEPresent() bool {
	slog.Warn("FUSE detection starting", "os", runtime.GOOS, "arch", runtime.GOARCH)

	var result bool
	switch runtime.GOOS {
	case "windows":
		// WinFSP registers its DLL in System32 when installed via the MSI/choco with admin.
		// Fallback: also check the WinFSP installation directory directly (e.g. when
		// installed without full system registration, or on ARM64 Windows).
		paths := []string{
			`C:\Windows\System32\winfsp-x64.dll`,
			`C:\Program Files (x86)\WinFsp\bin\winfsp-x64.dll`,
			`C:\Program Files\WinFsp\bin\winfsp-x64.dll`,
			`C:\Program Files (x86)\WinFsp\bin\winfsp-a64.dll`, // ARM64
		}
		for _, path := range paths {
			e := exists(path)
			slog.Warn("FUSE windows check", "path", path, "exists", e)
			if e {
				result = true
				break
			}
		}
	case "darwin":
		path1 := `/usr/local/lib/libfuse.dylib`
		path2 := `/Library/Filesystems/macfuse.fs`
		exists1, exists2 := exists(path1), exists(path2)
		slog.Warn("FUSE darwin check", "path1", path1, "exists1", exists1, "path2", path2, "exists2", exists2)
		result = exists1 || exists2
	case "linux":
		path1 := `/lib/x86_64-linux-gnu/libfuse.so.2`
		path2 := `/usr/lib/libfuse.so`
		path3 := `/usr/lib/x86_64-linux-gnu/libfuse3.so`
		exists1, exists2, exists3 := exists(path1), exists(path2), exists(path3)
		slog.Warn("FUSE linux check", "path1", path1, "exists1", exists1, "path2", path2, "exists2", exists2, "path3", path3, "exists3", exists3)
		result = exists1 || exists2 || exists3
	default:
		slog.Warn("FUSE unsupported OS", "os", runtime.GOOS)
		result = false
	}

	slog.Warn("FUSE detection result", "present", result)
	if !result {
		switch runtime.GOOS {
		case "darwin":
			slog.Warn("macFUSE not found. Install it for virtual folder support: https://macfuse.github.io/ or: brew install macfuse")
		case "linux":
			slog.Warn("FUSE not found. Install it for virtual folder support: sudo apt install libfuse-dev (Debian/Ubuntu) or sudo dnf install fuse-devel (Fedora)")
		case "windows":
			slog.Warn("WinFsp not found. Install it for virtual folder support: https://winfsp.dev/rel/ or: choco install winfsp")
		}
	}
	return result
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
