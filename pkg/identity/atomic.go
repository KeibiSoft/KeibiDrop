// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.
//
// ABOUTME: Atomic file-write helper for the identity package.
// ABOUTME: Writes to a .tmp sidecar then renames for crash-safe persistence.

package identity

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic writes data with write-then-rename so readers never see a partial file.
// It creates the parent directory with mode 0750 when absent.
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
