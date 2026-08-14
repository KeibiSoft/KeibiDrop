// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build linux

package service

import (
	"os"

	"golang.org/x/sys/unix"
)

// openReadNoAtime opens path read-only without updating its access time.
// O_NOATIME needs file ownership or CAP_FOWNER; on failure fall back to a
// plain open so serving still works.
func openReadNoAtime(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOATIME, 0) // #nosec G304
	if err == nil {
		return f, nil
	}
	return os.Open(path) // #nosec G304
}
