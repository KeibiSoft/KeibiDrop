// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !linux && !windows

package service

import "os"

// openReadNoAtime: macOS/iOS have no per-handle way to suppress access-time
// updates. Forensic guidance there: put the source on a noatime/read-only
// mount. Serving still works with a plain open.
func openReadNoAtime(path string) (*os.File, error) {
	return os.Open(path) // #nosec G304
}
