// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: A file manager's trash folder on the mount is refused with EPERM, so
// ABOUTME: Finder offers "delete immediately" and no trash move reaches the peer.

package filesystem

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func newWritableRoot(t *testing.T) *Dir {
	t.Helper()
	tmp := t.TempDir()
	root := &Dir{
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*Dir),
		RealPathOfFile:      tmp,
		LocalDownloadFolder: tmp,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root
	return root
}

// A move to Trash on the mount must fail at the trash folder, so it never reaches the peer.
func TestOSTrashOnTheMount_RefusedWithEPERM(t *testing.T) {
	d := newWritableRoot(t)
	require.Equal(t, -winfuse.EPERM, d.Mkdir("/.Trashes", 0o333), "macOS trash root")
	require.Equal(t, -winfuse.EPERM, d.Mkdir("/.Trashes/501", 0o700), "per-user trash folder")
	require.Equal(t, -winfuse.EPERM, d.Mkdir("/.Trash-1000", 0o700), "Linux trash folder")
	require.Equal(t, -winfuse.EPERM, d.Rename("/a.png", "/.Trashes/501/a.png"), "a move into a trash folder that already exists on disk")
	require.Equal(t, -winfuse.EPERM, d.Rename("/a.png", "/$RECYCLE.BIN/a.png"), "Windows recycle bin")
}
