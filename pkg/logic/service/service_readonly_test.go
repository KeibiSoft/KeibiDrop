// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests share_read_only origin enforcement: every peer mutation is
// ABOUTME: refused before disk, bytes and mtime stay intact, DISCONNECT passes.

//go:build !android

package service

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
)

func newReadOnlyFixture(t *testing.T) (*KeibidropServiceImpl, string) {
	t.Helper()
	tmpDir := t.TempDir()
	root := &filesystem.Dir{
		RemoteFilesLock:     sync.RWMutex{},
		RemoteFiles:         make(map[string]*filesystem.File),
		AfmLock:             sync.RWMutex{},
		AllFileMap:          make(map[string]*filesystem.File),
		OpenMapLock:         sync.RWMutex{},
		OpenFileHandlers:    make(map[uint64]*filesystem.HandleEntry),
		Adm:                 sync.RWMutex{},
		AllDirMap:           make(map[string]*filesystem.Dir),
		RealPathOfFile:      tmpDir,
		LocalDownloadFolder: tmpDir,
		PrefetchSem:         make(chan struct{}, 8),
	}
	root.Root = root

	svc := &KeibidropServiceImpl{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		SyncTracker:   synctracker.NewSyncTracker(),
		ShareReadOnly: true,
	}
	fsForSvc := &filesystem.FS{}
	fsForSvc.SetRoot(root)
	svc.SetFS(fsForSvc)
	return svc, tmpDir
}

func attrFor(size int64) *bindings.Attr {
	now := uint64(time.Now().UnixNano())
	return &bindings.Attr{Mode: 0o644, Size: size, ModificationTime: now, ChangeTime: now, BirthTime: now}
}

// TestShareReadOnly_RefusesEveryMutation: the full mutating notify matrix is
// refused with PermissionDenied and nothing lands in tracker or tree.
func TestShareReadOnly_RefusesEveryMutation(t *testing.T) {
	svc, _ := newReadOnlyFixture(t)

	mutations := []*bindings.NotifyRequest{
		{Type: bindings.NotifyType_ADD_FILE, Path: "new.txt", Attr: attrFor(4)},
		{Type: bindings.NotifyType_EDIT_FILE, Path: "victim.txt", Attr: attrFor(9)},
		{Type: bindings.NotifyType_REMOVE_FILE, Path: "victim.txt"},
		{Type: bindings.NotifyType_RENAME_FILE, Path: "renamed.txt", OldPath: "victim.txt", Attr: attrFor(4)},
		{Type: bindings.NotifyType_ADD_DIR, Path: "newdir"},
		{Type: bindings.NotifyType_REMOVE_DIR, Path: "somedir"},
	}
	for _, req := range mutations {
		_, err := svc.Notify(context.Background(), req)
		require.ErrorIs(t, err, ErrGRPCReadOnlyShare, "type %v must be refused", req.Type)
	}

	svc.SyncTracker.RemoteFilesMu.RLock()
	defer svc.SyncTracker.RemoteFilesMu.RUnlock()
	require.Empty(t, svc.SyncTracker.RemoteFiles, "refused notifies must not touch the tracker")
}

// TestShareReadOnly_DiskUntouched: a peer REMOVE/EDIT of a shared file leaves
// the on-disk bytes and mtime exactly as they were.
func TestShareReadOnly_DiskUntouched(t *testing.T) {
	svc, dir := newReadOnlyFixture(t)

	victim := filepath.Join(dir, "victim.txt")
	content := []byte("evidence bytes")
	require.NoError(t, os.WriteFile(victim, content, 0o644))
	before, err := os.Stat(victim)
	require.NoError(t, err)

	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_REMOVE_FILE, Path: "/victim.txt",
	})
	require.ErrorIs(t, err, ErrGRPCReadOnlyShare)
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE, Path: "/victim.txt", Attr: attrFor(3),
	})
	require.ErrorIs(t, err, ErrGRPCReadOnlyShare)

	// The REMOVE debounce buffers deletes for 1s; outlive it before checking.
	time.Sleep(1500 * time.Millisecond)

	after, err := os.Stat(victim)
	require.NoError(t, err, "file must still exist")
	got, err := os.ReadFile(victim)
	require.NoError(t, err)
	require.Equal(t, content, got, "bytes must be unchanged")
	require.Equal(t, before.ModTime(), after.ModTime(), "mtime must be unchanged")
}

// TestShareReadOnly_DisconnectStillPasses: the graceful disconnect control
// message is not a mutation and must keep working.
func TestShareReadOnly_DisconnectStillPasses(t *testing.T) {
	svc, _ := newReadOnlyFixture(t)
	disconnected := make(chan struct{}, 1)
	svc.OnDisconnect = func() { disconnected <- struct{}{} }

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_DISCONNECT,
	})
	require.NoError(t, err)
	require.Equal(t, "ok", resp.Status)
	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("OnDisconnect not called")
	}
}

// TestShareReadOnly_BatchRefused: the batch entry point is refused as a whole.
func TestShareReadOnly_BatchRefused(t *testing.T) {
	svc, _ := newReadOnlyFixture(t)
	_, err := svc.BatchNotify(context.Background(), &bindings.BatchNotifyRequest{
		Notifications: []*bindings.NotifyRequest{
			{Type: bindings.NotifyType_ADD_FILE, Path: "a.txt", Attr: attrFor(1)},
		},
		Seq: 1,
	})
	require.ErrorIs(t, err, ErrGRPCReadOnlyShare)
}

// TestShareReadOnly_OffAllowsMutations: flag off keeps today's behavior.
func TestShareReadOnly_OffAllowsMutations(t *testing.T) {
	svc, _ := newReadOnlyFixture(t)
	svc.ShareReadOnly = false

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: "ok.txt", Name: "ok.txt", Attr: attrFor(2),
	})
	require.NoError(t, err)
}
