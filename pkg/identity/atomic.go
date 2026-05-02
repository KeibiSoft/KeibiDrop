// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Atomic file-write helper for the identity package.
// ABOUTME: Writes to a .tmp sidecar then renames for crash-safe persistence.

package identity

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data to path using a write-then-rename strategy so
// that readers never see a partial file. The parent directory is created with
// mode 0750 if it does not exist. On rename failure a best-effort cleanup of
// the temporary file is attempted.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return fmt.Errorf("atomic write: create parent dir %q: %w", dir, err)
	}

	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, mode); err != nil {
		return fmt.Errorf("atomic write: write tmp file: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath) // best-effort cleanup
		return fmt.Errorf("atomic write: rename to %q: %w", path, err)
	}

	return nil
}
