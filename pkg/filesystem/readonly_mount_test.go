// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests mount_read_only: every local mutating FUSE op returns EROFS,
// ABOUTME: read intent passes the gate, peer-driven updates bypass it.

package filesystem

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func newReadOnlyRoot(t *testing.T) *Dir {
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
		MountReadOnly:       true,
	}
	root.Root = root
	return root
}

// TestMountReadOnly_AllWriteOpsReturnEROFS is the receiver-courtesy matrix:
// every local mutating op fails with EROFS before touching state.
func TestMountReadOnly_AllWriteOpsReturnEROFS(t *testing.T) {
	d := newReadOnlyRoot(t)

	require.Equal(t, -winfuse.EROFS, d.Chmod("/f.txt", 0o644), "Chmod")
	require.Equal(t, -winfuse.EROFS, d.Mkdir("/newdir", 0o755), "Mkdir")
	require.Equal(t, -winfuse.EROFS, d.Mknod("/dev0", 0o644, 0), "Mknod")
	require.Equal(t, -winfuse.EROFS, d.Rename("/a.txt", "/b.txt"), "Rename")
	require.Equal(t, -winfuse.EROFS, d.Rmdir("/somedir"), "Rmdir")
	require.Equal(t, -winfuse.EROFS, d.Truncate("/f.txt", 0, 0), "Truncate")
	require.Equal(t, -winfuse.EROFS, d.Unlink("/f.txt"), "Unlink")
	require.Equal(t, -winfuse.EROFS, d.Utimens("/f.txt", nil), "Utimens")
	require.Equal(t, -winfuse.EROFS, d.Write("/f.txt", []byte("x"), 0, 0), "Write")
	require.Equal(t, -winfuse.EROFS, d.Setxattr("/f.txt", "user.k", []byte("v"), 0), "Setxattr")

	errc, _ := d.Create("/new.txt", winfuse.O_CREAT|winfuse.O_RDWR, 0o644)
	require.Equal(t, -winfuse.EROFS, errc, "Create")

	errc, _ = d.Open("/f.txt", winfuse.O_RDWR)
	require.Equal(t, -winfuse.EROFS, errc, "Open O_RDWR")
	errc, _ = d.Open("/f.txt", winfuse.O_WRONLY)
	require.Equal(t, -winfuse.EROFS, errc, "Open O_WRONLY")
}

// TestMountReadOnly_ReadIntentPasses: a read-only open passes the gate (the
// miss then fails with a non-EROFS code, proving reads are not blocked).
func TestMountReadOnly_ReadIntentPasses(t *testing.T) {
	d := newReadOnlyRoot(t)
	errc, _ := d.Open("/absent.txt", 0) // O_RDONLY
	require.NotEqual(t, -winfuse.EROFS, errc, "read-only open must not hit the EROFS gate")
}

// TestMountReadOnly_PeerUpdatesBypass: peer-driven tree changes are not local
// writes; the read-only mount must keep receiving them.
func TestMountReadOnly_PeerUpdatesBypass(t *testing.T) {
	d := newReadOnlyRoot(t)

	require.Equal(t, 0, d.MkdirFromPeer("/from-peer", 0o755), "MkdirFromPeer")

	stat := &winfuse.Stat_t{Mode: 0o644 | 0o100000, Size: 42, Nlink: 1}
	require.NoError(t, d.AddRemoteFileWithBase(nil, "/from-peer/file.bin", "file.bin", stat, 0))

	d.RemoteFilesLock.RLock()
	_, ok := d.RemoteFiles["/from-peer/file.bin"]
	d.RemoteFilesLock.RUnlock()
	require.True(t, ok, "peer-announced file must appear in the tree")
}

// TestMountReadOnly_OffKeepsWrites: flag off keeps today's behavior.
func TestMountReadOnly_OffKeepsWrites(t *testing.T) {
	d := newReadOnlyRoot(t)
	d.MountReadOnly = false

	require.Equal(t, 0, d.Mkdir("/writable", 0o755), "Mkdir with flag off")
}
