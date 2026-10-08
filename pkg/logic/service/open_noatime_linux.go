// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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
