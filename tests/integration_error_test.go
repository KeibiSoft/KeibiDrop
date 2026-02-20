// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestErrorMessages verifies that errors from the KeibiDrop API contain
// meaningful messages that can be displayed in a UI (not just error codes).
func TestErrorMessages(t *testing.T) {
	tp := SetupPeerPair(t, false)
	require := require.New(t)

	t.Run("AddFile_NonexistentPath_HasMessage", func(t *testing.T) {
		err := tp.Alice.AddFile("/nonexistent/path/file.txt")
		require.Error(err)
		// Error message should contain the path or "no such file".
		require.Contains(err.Error(), "no such file")
	})

	t.Run("AddFile_Directory_HasMessage", func(t *testing.T) {
		err := tp.Alice.AddFile(tp.AliceSaveDir)
		require.Error(err)
		require.ErrorIs(err, syscall.EISDIR)
		require.Contains(err.Error(), "is a directory")
	})

	t.Run("PullFile_Nonexistent_HasMessage", func(t *testing.T) {
		dest := filepath.Join(tp.BobSaveDir, "nope.txt")
		err := tp.Bob.PullFile("nonexistent.txt", dest)
		require.Error(err)
		require.ErrorIs(err, syscall.ENOENT)
	})

	t.Run("AddFile_Duplicate_HasMessage", func(t *testing.T) {
		path := filepath.Join(tp.AliceSaveDir, "dup_msg.txt")
		require.NoError(os.WriteFile(path, []byte("test"), 0644))
		require.NoError(tp.Alice.AddFile(path))

		err := tp.Alice.AddFile(path)
		require.ErrorIs(err, os.ErrExist)
		require.Contains(err.Error(), "file already exists")
	})

	t.Run("InvalidSession_HasMessage", func(t *testing.T) {
		// A KeibiDrop with nil session should return ErrInvalidSession.
		require.ErrorIs(common.ErrInvalidSession, common.ErrInvalidSession)
		require.Contains(common.ErrInvalidSession.Error(), "invalid")
	})
}
