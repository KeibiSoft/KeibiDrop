// ABOUTME: Tests for the Android Notify handler: ADD stale-guard, REMOVE
// ABOUTME: debounce, EDIT/RENAME resilience, and OnEvent emission.

//go:build android

package service

import (
	"context"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService() *KeibidropServiceImpl {
	return &KeibidropServiceImpl{
		Logger:      testkit.DiscardLogger(),
		SyncTracker: synctracker.NewSyncTracker(),
	}
}

func TestNotify_EditFile_CreatesEntryWhenMissing(t *testing.T) {
	svc := newTestService()

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE,
		Path: "docs/readme.md",
		Name: "readme.md",
		Attr: &bindings.Attr{
			Size:             4096,
			ModificationTime: 1000,
			BirthTime:        900,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	f, ok := svc.SyncTracker.RemoteFiles["docs/readme.md"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.True(t, ok, "EDIT_FILE for unknown path should create entry")
	assert.Equal(t, "readme.md", f.Name)
	assert.Equal(t, "docs/readme.md", f.RelativePath)
	assert.Equal(t, uint64(4096), f.Size)
	assert.Equal(t, uint64(1000), f.LastEditTime)
	assert.Equal(t, uint64(900), f.CreatedTime)
}

func TestNotify_EditFile_UpdatesExistingEntry(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["test.txt"] = &synctracker.File{
		Name:         "test.txt",
		RelativePath: "test.txt",
		Size:         100,
	}

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE,
		Path: "test.txt",
		Name: "test.txt",
		Attr: &bindings.Attr{
			Size:             200,
			ModificationTime: 2000,
			BirthTime:        500,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	f := svc.SyncTracker.RemoteFiles["test.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.NotNil(t, f)
	assert.Equal(t, uint64(200), f.Size)
	assert.Equal(t, uint64(2000), f.LastEditTime)
}

func TestNotify_Remove_BufferedThenCancelledByAdd(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["f.txt"] = &synctracker.File{Name: "f.txt", RelativePath: "f.txt", Size: 100}

	// REMOVE is buffered, not applied immediately.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{Type: bindings.NotifyType_REMOVE_FILE, Path: "f.txt"})
	require.NoError(t, err)
	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok := svc.SyncTracker.RemoteFiles["f.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()
	assert.True(t, ok, "REMOVE should be buffered, file still present")

	// An ADD for the same path within the window cancels the buffered remove.
	_, err = svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: "f.txt", Name: "f.txt",
		Attr: &bindings.Attr{Size: 100, ModificationTime: 1},
	})
	require.NoError(t, err)
	time.Sleep(1200 * time.Millisecond)
	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok = svc.SyncTracker.RemoteFiles["f.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()
	assert.True(t, ok, "ADD within the window must cancel the buffered remove")
}

func TestNotify_Remove_ExecutesAfterWindow(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["g.txt"] = &synctracker.File{Name: "g.txt", RelativePath: "g.txt", Size: 100}

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{Type: bindings.NotifyType_REMOVE_FILE, Path: "g.txt"})
	require.NoError(t, err)
	time.Sleep(1200 * time.Millisecond)
	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok := svc.SyncTracker.RemoteFiles["g.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()
	assert.False(t, ok, "buffered remove must execute after the window")
}

func TestNotify_RenameFile_UpdatesSizeFromAttr(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["old.txt"] = &synctracker.File{
		Name:         "old.txt",
		RelativePath: "old.txt",
		Size:         100,
	}

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: "old.txt",
		Path:    "new.txt",
		Attr: &bindings.Attr{
			Size:             120,
			ModificationTime: 3000,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	_, oldExists := svc.SyncTracker.RemoteFiles["old.txt"]
	f, newExists := svc.SyncTracker.RemoteFiles["new.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	assert.False(t, oldExists, "old path should be deleted")
	require.True(t, newExists, "new path should exist")
	assert.Equal(t, uint64(120), f.Size, "size should be updated from attr")
	assert.Equal(t, "new.txt", f.Name)
	assert.Equal(t, "new.txt", f.RelativePath)
}

func TestNotify_RenameFile_CreatesEntryWhenOldPathMissing(t *testing.T) {
	svc := newTestService()

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: "temp-1234.tmp",
		Path:    "final.txt",
		Attr: &bindings.Attr{
			Size:             5000,
			ModificationTime: 4000,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	f, ok := svc.SyncTracker.RemoteFiles["final.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.True(t, ok, "RENAME with untracked old path should create entry")
	assert.Equal(t, "final.txt", f.Name)
	assert.Equal(t, "final.txt", f.RelativePath)
	assert.Equal(t, uint64(5000), f.Size)
	assert.Equal(t, uint64(4000), f.LastEditTime)
}

func TestNotify_RenameFile_NoAttr_SilentlyDropsUntracked(t *testing.T) {
	svc := newTestService()

	resp, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type:    bindings.NotifyType_RENAME_FILE,
		OldPath: "unknown.tmp",
		Path:    "final.txt",
	})

	require.NoError(t, err)
	require.NotNil(t, resp)

	svc.SyncTracker.RemoteFilesMu.RLock()
	_, ok := svc.SyncTracker.RemoteFiles["final.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	assert.False(t, ok, "RENAME with no attr and untracked old path should not create entry")
}

func TestNotify_AddFile_EmitsFileArrivedEvent(t *testing.T) {
	svc := newTestService()
	var received string
	svc.OnEvent = func(ev string) { received = ev }

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE,
		Path: "photo.jpg",
		Name: "photo.jpg",
		Attr: &bindings.Attr{Size: 1024},
	})

	require.NoError(t, err)
	assert.Equal(t, "file_arrived:photo.jpg:1024", received)
}

func TestNotify_AddFile_ExistingUpdate_NoEvent(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["photo.jpg"] = &synctracker.File{
		Name: "photo.jpg", RelativePath: "photo.jpg", Size: 512,
	}
	var received string
	svc.OnEvent = func(ev string) { received = ev }

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE,
		Path: "photo.jpg",
		Name: "photo.jpg",
		Attr: &bindings.Attr{Size: 1024},
	})

	require.NoError(t, err)
	assert.Empty(t, received, "updating existing file should not emit file_arrived")
}

func TestNotify_EditFile_EmitsFileArrivedEvent(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["doc.txt"] = &synctracker.File{
		Name: "doc.txt", RelativePath: "doc.txt", Size: 100,
	}
	var received string
	svc.OnEvent = func(ev string) { received = ev }

	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_EDIT_FILE,
		Path: "doc.txt",
		Name: "doc.txt",
		Attr: &bindings.Attr{Size: 200},
	})

	require.NoError(t, err)
	assert.Equal(t, "file_arrived:doc.txt:200", received)
}

func TestNotify_AddFile_RejectsStaleSmallerOlder(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["f.txt"] = &synctracker.File{
		Name: "f.txt", RelativePath: "f.txt", Size: 100, LastEditTime: 2000,
	}

	// A stale temp-file ADD: smaller AND older than what we have. Reject it.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: "f.txt", Name: "f.txt",
		Attr: &bindings.Attr{Size: 50, ModificationTime: 1000},
	})
	require.NoError(t, err)

	svc.SyncTracker.RemoteFilesMu.RLock()
	f := svc.SyncTracker.RemoteFiles["f.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.NotNil(t, f)
	assert.Equal(t, uint64(100), f.Size, "stale smaller+older ADD must be ignored")
	assert.Equal(t, uint64(2000), f.LastEditTime)
}

func TestNotify_AddFile_AcceptsSmallerNewer(t *testing.T) {
	svc := newTestService()
	svc.SyncTracker.RemoteFiles["f.txt"] = &synctracker.File{
		Name: "f.txt", RelativePath: "f.txt", Size: 100, LastEditTime: 1000,
	}

	// A real shrink (e.g. git HEAD 100->50) carries a newer mtime. Accept it.
	_, err := svc.Notify(context.Background(), &bindings.NotifyRequest{
		Type: bindings.NotifyType_ADD_FILE, Path: "f.txt", Name: "f.txt",
		Attr: &bindings.Attr{Size: 50, ModificationTime: 2000},
	})
	require.NoError(t, err)

	svc.SyncTracker.RemoteFilesMu.RLock()
	f := svc.SyncTracker.RemoteFiles["f.txt"]
	svc.SyncTracker.RemoteFilesMu.RUnlock()

	require.NotNil(t, f)
	assert.Equal(t, uint64(50), f.Size, "real shrink with newer mtime must be accepted")
	assert.Equal(t, uint64(2000), f.LastEditTime)
}
