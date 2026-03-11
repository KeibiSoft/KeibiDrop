// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

// SecureJoin resolves path relative to base and verifies the result stays within
// base. Returns an error if the resolved path escapes base (KD-SEC-2026-004).
// Symlinks are resolved when the path exists to prevent symlink-escape attacks.
//
// Threat model: local write access to base is assumed to be trusted. A TOCTOU
// window exists between EvalSymlinks validation and the caller's subsequent
// syscall; closing it requires kernel-level openat(2)/O_NOFOLLOW chains, which
// is out of scope for this guard. In KeibiDrop, the FUSE Symlink handler returns
// EPERM so no remote peer can introduce symlinks inside base.
func SecureJoin(base, path string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path of base: %w", err)
	}

	// Join and Clean will handle leading slashes by making them relative to absBase.
	// e.g. Join("/base", "/foo") -> "/base/foo"
	result := filepath.Clean(filepath.Join(absBase, path))

	// Verify the result is still within absBase before resolving symlinks.
	if result != absBase && !strings.HasPrefix(result, absBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes base directory %q", path, absBase)
	}

	// Resolve symlinks if the path exists. A symlink inside base could point
	// to a location outside base, bypassing the prefix check above.
	// If EvalSymlinks fails (path does not exist yet), the initial check suffices.
	if resolved, evalErr := filepath.EvalSymlinks(result); evalErr == nil {
		absResolved, absErr := filepath.Abs(resolved)
		if absErr != nil {
			return "", fmt.Errorf("failed to get absolute path of resolved symlink: %w", absErr)
		}
		if absResolved != absBase && !strings.HasPrefix(absResolved, absBase+string(os.PathSeparator)) {
			return "", fmt.Errorf("path %q escapes base directory via symlink", path)
		}
		return absResolved, nil
	}

	return result, nil
}

func convertOsErrToSyscallErrno(name string, err error) syscall.Errno {
	if err == nil {
		return 0
	}

	e := os.NewSyscallError(name, err)
	var targetErr syscall.Errno

	ok := errors.As(e, &targetErr)
	if !ok {
		slog.Warn("FUSE error conversion - unknown error type", "syscall", name, "error", err, "fallback", "EIO")
		return syscall.EIO
	}

	// Only log unexpected errors, not common expected ones
	// ENOENT (no such file) is normal for xattr lookups and Getattr on non-existent files
	// Errno 93 (ENOATTR on macOS) is normal for xattr lookups
	if targetErr != syscall.ENOENT && int(targetErr) != 93 {
		slog.Warn("FUSE error conversion", "syscall", name, "error", err, "errno", targetErr)
	}
	// cgoFuse uses -errno
	return -targetErr
}

func isModificationTimeNewer(a, b *winfuse.Stat_t) bool {
	return a.Mtim.Time().After(b.Mtim.Time())
}

func getNameFromPath(path string) string {
	aux := strings.Split(path, "/")
	if len(aux) == 0 {
		return path
	}

	return aux[len(aux)-1]
}

// remoteChildrenForDir returns the direct file and directory children of dirPath
// found within the remoteFiles map. Prevents phantom entries at wrong directory level.
func remoteChildrenForDir(remoteFiles map[string]*File, dirPath string) (files map[string]struct{}, dirs map[string]struct{}) {
	files = make(map[string]struct{})
	dirs = make(map[string]struct{})

	var prefix string
	if dirPath == "/" || dirPath == "" {
		prefix = "/"
	} else {
		prefix = strings.TrimRight(dirPath, "/") + "/"
	}

	for k := range remoteFiles {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if rest == "" {
			continue
		}
		slashIdx := strings.Index(rest, "/")
		if slashIdx == -1 {
			files[rest] = struct{}{} // direct file child
		} else {
			dirs[rest[:slashIdx]] = struct{}{} // intermediate directory
		}
	}
	return
}

func getPathWithoutName(path string) string {
	aux := strings.Split(path, "/")
	if len(aux) == 0 {
		return path
	}

	return strings.Join(aux[:len(aux)-1], "/")
}
