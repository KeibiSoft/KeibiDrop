// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

//go:build !linux && !windows

package service

import "os"

// openReadNoAtime: macOS/iOS have no per-handle way to suppress access-time
// updates. Forensic guidance there: put the source on a noatime/read-only
// mount. Serving still works with a plain open.
func openReadNoAtime(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304
}

// openReadShared opens path read-only to serve a peer. POSIX lets the owner
// delete or rename a file a reader holds open.
func openReadShared(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304
}
