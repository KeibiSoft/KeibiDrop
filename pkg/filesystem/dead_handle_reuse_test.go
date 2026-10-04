//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// A handle closed under its open count must not be handed out again. On
// Windows a rename closes every handle of both paths before it runs, and the
// count stays up: the next opens reused the dead handle, every write went to
// the miss path, and an in-place edit was never announced
// (TestFUSEtoFUSE_BidirectionalEditPatterns/InPlaceSameSize, 4 Oct).
func TestOpenEx_HandleClosedUnderItsCountIsNotReused(t *testing.T) {
	save := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(save, "doc.bin"), make([]byte, 8192), 0o644))
	d := newTestDir(save)

	first := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, 0, d.OpenEx("/doc.bin", first))

	// What Rename does on Windows before platRename: close and drop the
	// handle, leave the count.
	d.OpenMapLock.Lock()
	require.NoError(t, platClose(d.OpenFileHandlers[first.Fh].FD))
	delete(d.OpenFileHandlers, first.Fh)
	d.OpenMapLock.Unlock()

	second := &winfuse.FileInfo_t{Flags: os.O_WRONLY}
	require.Equal(t, 0, d.OpenEx("/doc.bin", second))
	d.OpenMapLock.RLock()
	_, live := d.OpenFileHandlers[second.Fh]
	d.OpenMapLock.RUnlock()
	require.True(t, live, "the open must get a live handle, not the dropped one")

	require.Equal(t, 4, d.Write("/doc.bin", []byte("edit"), 0, second.Fh))
	d.AfmLock.RLock()
	f := d.AllFileMap["/doc.bin"]
	d.AfmLock.RUnlock()
	require.NotNil(t, f)
	f.metaMu.RLock()
	edited := f.HadEdits
	f.metaMu.RUnlock()
	require.True(t, edited, "the write must open an edit session, so the close announces it")
	require.Equal(t, 0, d.Release("/doc.bin", second.Fh))
	require.Zero(t, f.openFileCounter.CountOpenDescriptors(), "the count must end at zero after the last release")
}
