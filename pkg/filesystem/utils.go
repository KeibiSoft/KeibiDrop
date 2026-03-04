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
func SecureJoin(base, path string) (string, error) {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("failed to get absolute path of base: %w", err)
	}

	result := filepath.Clean(filepath.Join(absBase, path))
	if result != absBase && !strings.HasPrefix(result, absBase+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes base directory %q", path, absBase)
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

func getPathWithoutName(path string) string {
	aux := strings.Split(path, "/")
	if len(aux) == 0 {
		return path
	}

	return strings.Join(aux[:len(aux)-1], "/")
}
