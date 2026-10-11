//go:build !android

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// Unmount writes every sidecar still on its 1 s timer. A timer that fired after
// the unmount wrote .kdbitmap files into a save folder the caller was removing
// (TestUXLatency_ManySmallFiles on Windows, 4 Oct), and one pending when the
// process exits is lost.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFlushPendingSidecarsWritesTheWaitingOnes(t *testing.T) {
	d, saveDir, _, _ := newFillDir(t, 64*1024)
	d.RemoteFilesLock.RLock()
	f := d.RemoteFiles["/big.bin"]
	d.RemoteFilesLock.RUnlock()
	require.NotNil(t, f)
	sidecar := BitmapPath(filepath.Join(saveDir, "big.bin"))

	// A landing writes into the cache copy first: the sidecar names its bytes,
	// and a sidecar without its copy is not written.
	require.NoError(t, os.WriteFile(filepath.Join(saveDir, "big.bin"), make([]byte, 64*1024), 0o644))
	f.noteLanded() // what a warmed sibling's landing does: no handle, no Release
	_, err := os.Stat(sidecar)
	require.True(t, os.IsNotExist(err), "precondition: the sidecar waits on its timer")

	d.flushPendingSidecars()

	_, err = os.Stat(sidecar)
	require.NoError(t, err, "the waiting sidecar is written at once")
	f.sidecarMu.Lock()
	pending := f.sidecarTimer != nil
	f.sidecarMu.Unlock()
	require.False(t, pending, "no timer may fire after the flush")
}
