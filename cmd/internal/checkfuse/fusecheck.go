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
		// The WinFSP installer registers its DLL in System32 only with admin rights.
		// Also check the install directory for partial installs and ARM64 Windows.
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
		found := findLinuxFUSE()
		slog.Warn("FUSE linux check", "found", found)
		result = found != ""
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
			slog.Warn("FUSE not found. Install it for virtual folder support: sudo apt install fuse3 libfuse3-3 (Debian/Ubuntu) or sudo dnf install fuse3 fuse3-libs (Fedora)")
		case "windows":
			slog.Warn("WinFsp not found. Install it for virtual folder support: https://winfsp.dev/rel/ or: choco install winfsp")
		}
	}
	return result
}

// linuxFUSELibs lists the runtime sonames, FUSE 3 first.
// The bare libfuse3.so symlink ships only in libfuse3-dev, so do not match it.
var linuxFUSELibs = []string{"libfuse3.so.3", "libfuse.so.2"}

// linuxLibDirs returns the search directories, multiarch tuple first.
// GOARCH selects the tuple so arm64 and armv7 also resolve.
func linuxLibDirs() []string {
	tuple := map[string]string{
		"amd64": "x86_64-linux-gnu",
		"arm64": "aarch64-linux-gnu",
		"arm":   "arm-linux-gnueabihf",
		"386":   "i386-linux-gnu",
	}[runtime.GOARCH]

	dirs := make([]string, 0, 6)
	if tuple != "" {
		dirs = append(dirs, "/lib/"+tuple, "/usr/lib/"+tuple)
	}
	return append(dirs, "/lib", "/usr/lib", "/usr/lib64", "/usr/local/lib")
}

// findLinuxFUSE returns the path of the first FUSE runtime library present, or "".
func findLinuxFUSE() string {
	for _, lib := range linuxFUSELibs {
		for _, dir := range linuxLibDirs() {
			if p := dir + "/" + lib; exists(p) {
				return p
			}
		}
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
