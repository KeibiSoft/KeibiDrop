// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: A read through the mount inside the 1000ms remove window makes and fills the
// ABOUTME: cache file. That is not a local write: the peer's delete must still land.
//go:build !android

package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// sliceProvider serves one file's bytes, as a live session does.
type sliceProvider struct{ data []byte }

func (p *sliceProvider) OpenRemoteFile(context.Context, uint64, string) (types.RemoteFileStream, error) {
	return &sliceStream{data: p.data}, nil
}

func (p *sliceProvider) StreamFile(context.Context, string, uint64) (types.StreamFileReceiver, error) {
	return &sliceReceiver{data: p.data}, nil
}

type sliceStream struct{ data []byte }

func (s *sliceStream) ReadAt(_ context.Context, offset, size int64) ([]byte, error) {
	if offset >= int64(len(s.data)) {
		return []byte{}, nil
	}
	end := min(offset+size, int64(len(s.data)))
	return append([]byte(nil), s.data[offset:end]...), nil
}

func (s *sliceStream) Close() error { return nil }

type sliceReceiver struct {
	data []byte
	sent bool
}

func (r *sliceReceiver) Recv() ([]byte, uint64, uint64, error) {
	if r.sent {
		return nil, 0, uint64(len(r.data)), io.EOF
	}
	r.sent = true
	return r.data, 0, uint64(len(r.data)), nil
}

// The peer's vim deletes its swap file; the reader's vim opens that file inside
// the window to look for a recovery. Before the fix the newer cache mtime
// cancelled the delete, the swap stayed on the reader, and the next vim there
// stopped on E325 (TestFUSEtoFUSE_EditorSaves/VimTurnTaking, 4 Oct).
func TestRemoveFile_FuseMode_ReadDuringWindowDoesNotCancelTheRemove(t *testing.T) {
	svc, root, tmpDir := newFuseServiceForRemoveTests(t)
	const filePath = "/.note.txt.swp"
	data := []byte("the peer's vim swap file")
	root.SetStreamProvider(func() types.FileStreamProvider { return &sliceProvider{data: data} })

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: filePath, Name: ".note.txt.swp",
		Attr: &bindings.Attr{Size: int64(len(data)), ModificationTime: 1000000, Mode: 0o644},
	})
	require.NoError(t, err)
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: filePath,
	})
	require.NoError(t, err)

	// Inside the window: a read-only open and a read through the mount.
	time.Sleep(50 * time.Millisecond)
	var st winfuse.Stat_t
	require.Equal(t, 0, root.Getattr(filePath, &st, 0))
	fi := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, 0, root.OpenEx(filePath, fi))
	buf := make([]byte, len(data))
	require.Equal(t, len(data), root.Read(filePath, buf, 0, fi.Fh))
	require.Equal(t, data, buf)
	require.Equal(t, 0, root.Release(filePath, fi.Fh))
	cachePath := filepath.Join(tmpDir, filePath)
	_, statErr := os.Stat(cachePath)
	require.NoError(t, statErr, "precondition: the read made the cache file")

	testkit.Eventually(t, 5*time.Second, 25*time.Millisecond, func() bool {
		root.RemoteFilesLock.RLock()
		_, listed := root.RemoteFiles[filePath]
		root.RemoteFilesLock.RUnlock()
		root.AfmLock.RLock()
		_, inAfm := root.AllFileMap[filePath]
		root.AfmLock.RUnlock()
		_, err := os.Stat(cachePath)
		return !listed && !inAfm && os.IsNotExist(err)
	}, "the peer's delete must land after a read inside the window")
}
