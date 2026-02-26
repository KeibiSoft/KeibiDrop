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

// IsFUSEPresent performs a platform-specific check to see if the required
// FUSE libraries/drivers are installed on the system.
func IsFUSEPresent() bool {
	slog.Warn("FUSE detection starting", "os", runtime.GOOS, "arch", runtime.GOARCH)

	result := isFUSEPresent()

	slog.Warn("FUSE detection result", "present", result)
	return result
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
