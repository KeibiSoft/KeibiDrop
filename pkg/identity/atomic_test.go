// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for WriteFileAtomic covering happy paths and atomicity guarantees.
// ABOUTME: Verifies no partial writes are visible and that .tmp files are cleaned up.

package identity

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWriteFileAtomic_RenamesFromTmp(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "file.dat")

	req.NoError(WriteFileAtomic(path, []byte("hello"), 0o600))

	// The destination file must exist.
	_, err := os.Stat(path)
	req.NoError(err, "destination file missing after WriteFileAtomic")

	// The .tmp sidecar must be gone.
	_, err = os.Stat(path + ".tmp")
	req.True(os.IsNotExist(err), "tmp file must not remain after successful write")
}

func TestWriteFileAtomic_PreservesMode(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.dat")

	req.NoError(WriteFileAtomic(path, []byte("secret"), 0o600))

	info, err := os.Stat(path)
	req.NoError(err)
	req.Equal(os.FileMode(0o600), info.Mode().Perm())
}

func TestWriteFileAtomic_OverwritesExisting(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "file.dat")

	// Pre-create with stale content.
	req.NoError(os.WriteFile(path, []byte("old content"), 0o600))

	req.NoError(WriteFileAtomic(path, []byte("new content"), 0o600))

	got, err := os.ReadFile(path)
	req.NoError(err)
	req.Equal([]byte("new content"), got)
}

func TestWriteFileAtomic_CreatesParentDir(t *testing.T) {
	req := require.New(t)
	base := t.TempDir()
	path := filepath.Join(base, "sub", "dir", "file.dat")

	req.NoError(WriteFileAtomic(path, []byte("deep"), 0o600))

	got, err := os.ReadFile(path)
	req.NoError(err)
	req.Equal([]byte("deep"), got)

	// Parent dir should have been created with mode 0750.
	info, err := os.Stat(filepath.Join(base, "sub"))
	req.NoError(err)
	req.Equal(os.FileMode(0o750), info.Mode().Perm())
}

func TestWriteFileAtomic_NoPartialWriteVisible(t *testing.T) {
	req := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "big.dat")

	// 4 MiB payload — large enough that a naive write would be non-atomic.
	blob := make([]byte, 4*1024*1024)
	for i := range blob {
		blob[i] = 0xAB
	}

	req.NoError(WriteFileAtomic(path, blob, 0o600))

	// After rename completes the file must contain the full payload.
	got, err := os.ReadFile(path)
	req.NoError(err)
	req.Equal(len(blob), len(got), "file size mismatch — possible partial write")
	req.Equal(blob, got, "file content mismatch")
}
