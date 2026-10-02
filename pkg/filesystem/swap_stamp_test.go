// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func lastRename(events []types.FileEvent, path string) *types.FileEvent {
	var out *types.FileEvent
	for i := range events {
		if events[i].Action == types.RenameFile && events[i].Path == path {
			out = &events[i]
		}
	}
	return out
}

func announcedNs(f *File) int64 {
	f.metaMu.Lock()
	defer f.metaMu.Unlock()
	return f.LastAnnouncedMtimeNs
}

// A swap onto a target whose watermark W sits above the working copy's stamp
// (a peer swap landed after the app wrote its copy) announces a stamp above W,
// and the moved object's identity, its last announce and the Attr all carry
// that one stamp. Base: the version this peer held, not W.
func TestRename_SwapStampAboveSeenWatermark(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	target := seedLocalFile(t, d, "/doc.txt", "held-v1")
	held := localIdentity(target)

	// The peer's swap: accepted metadata only, W above anything local. The
	// acceptance path tracks the object in RemoteFiles too (AddRemoteFileWithBase).
	w := time.Now().Add(2 * time.Second).UnixNano()
	target.metaMu.Lock()
	target.RemoteMtimeNs = w
	target.LocalNewer = false
	target.metaMu.Unlock()
	d.RemoteFilesLock.Lock()
	d.RemoteFiles["/doc.txt"] = target
	d.RemoteFilesLock.Unlock()

	// The app's working copy, written now: both its disk mtime and its
	// in-memory stamp sit below W.
	tmp := seedLocalFile(t, d, "/doc.txt.tmp", "working-copy")
	require.Less(t, localIdentity(tmp), w)

	require.Equal(t, 0, d.Rename("/doc.txt.tmp", "/doc.txt"))

	ev := lastRename(snapshot(), "/doc.txt")
	require.NotNil(t, ev)
	require.NotNil(t, ev.Attr)
	stamp := int64(ev.Attr.ModificationTime)
	require.Greater(t, stamp, w, "the swap must announce a stamp above the accepted peer version")
	require.Equal(t, held, ev.BaseMtimeNs, "the base is the version this peer held, not the watermark")

	d.AfmLock.RLock()
	moved := d.AllFileMap["/doc.txt"]
	d.AfmLock.RUnlock()
	require.Same(t, tmp, moved)
	require.Equal(t, stamp, localIdentity(moved), "the in-memory identity must be the announced stamp")
	require.Equal(t, stamp, announcedNs(moved), "LastAnnouncedMtimeNs must be the announced stamp")
	d.RemoteFilesLock.RLock()
	tracked := d.RemoteFiles["/doc.txt"]
	d.RemoteFilesLock.RUnlock()
	require.Same(t, moved, tracked, "the moved object replaces the tracked target")
	require.Equal(t, w, tracked.RemoteMtimeNs, "the watermark transplants to the moved object")
}

// Without a newer peer version the swap announces the working copy's own
// identity: the in-memory stamp Write took, never the disk mtime below it.
// The disk mtime is left alone.
func TestRename_SwapStampKeepsInMemoryIdentity(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	seedLocalFile(t, d, "/doc.txt", "v1")
	tmp := seedLocalFile(t, d, "/doc.txt.tmp", "working-copy")
	mem := localIdentity(tmp)

	// Push the disk mtime of the working copy well below the in-memory stamp,
	// as a coarse kernel clock does.
	disk := filepath.Join(d.LocalDownloadFolder, "doc.txt.tmp")
	old := time.Unix(0, mem).Add(-5 * time.Millisecond)
	require.NoError(t, os.Chtimes(disk, old, old))

	require.Equal(t, 0, d.Rename("/doc.txt.tmp", "/doc.txt"))

	ev := lastRename(snapshot(), "/doc.txt")
	require.NotNil(t, ev)
	stamp := int64(ev.Attr.ModificationTime)
	require.GreaterOrEqual(t, stamp, mem, "the announce must not fall below the identity Write stamped")
	require.Equal(t, stamp, localIdentity(tmp))
	require.Equal(t, stamp, announcedNs(tmp))
	st, err := os.Stat(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, old.UnixNano(), st.ModTime().UnixNano(), "the disk mtime is not rewritten on the rename path")
}

// A plain rename to a new name, no target: the announce is the file's own
// identity and the base is 0 (untracked target).
func TestRename_PlainRenameAnnouncesOwnStamp(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/a.txt", "content")
	mem := localIdentity(f)
	require.Equal(t, 0, d.Rename("/a.txt", "/b.txt"))
	ev := lastRename(snapshot(), "/b.txt")
	require.NotNil(t, ev)
	require.GreaterOrEqual(t, int64(ev.Attr.ModificationTime), mem)
	require.Equal(t, int64(0), ev.BaseMtimeNs)
}

// A directory rename keeps its disk stat: no file stamp logic applies.
func TestRename_DirectoryKeepsDiskStat(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	require.Equal(t, 0, d.Mkdir("/sub", 0o755))
	require.Equal(t, 0, d.Rename("/sub", "/moved"))
	ev := lastRename(snapshot(), "/moved")
	require.NotNil(t, ev)
	st, err := os.Stat(filepath.Join(d.LocalDownloadFolder, "moved"))
	require.NoError(t, err)
	require.Equal(t, uint64(st.ModTime().UnixNano()), ev.Attr.ModificationTime)
	_ = winfuse.S_IFDIR
}
