// ABOUTME: Tests for UnshareFile — removes file from SyncTracker, errors on missing.
// ABOUTME: Covers the tracker mutation path; gRPC notification is fire-and-forget.
package common

import (
	"log/slog"
	"os"
	"testing"

	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
)

func newUnshareTestKD() *KeibiDrop {
	kd := &KeibiDrop{
		logger:      slog.New(slog.NewTextHandler(os.Stderr, nil)),
		SyncTracker: synctracker.NewSyncTracker(),
	}
	return kd
}

func TestUnshareFile_RemovesFromTracker(t *testing.T) {
	kd := newUnshareTestKD()
	kd.SyncTracker.LocalFiles["photo.jpg"] = &synctracker.File{
		Name:           "photo.jpg",
		RealPathOfFile: "/tmp/photo.jpg",
		Size:           1024,
	}

	require.NoError(t, kd.UnshareFile("photo.jpg"))

	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["photo.jpg"]
	kd.SyncTracker.LocalFilesMu.RUnlock()

	require.False(t, exists, "file should be removed from LocalFiles")
}

func TestUnshareFile_ErrorOnMissing(t *testing.T) {
	kd := newUnshareTestKD()

	err := kd.UnshareFile("nonexistent.txt")
	require.Error(t, err, "expected error for unsharing missing file")
}

func TestUnshareFile_DoesNotAffectOtherFiles(t *testing.T) {
	kd := newUnshareTestKD()
	kd.SyncTracker.LocalFiles["keep.txt"] = &synctracker.File{
		Name: "keep.txt",
	}
	kd.SyncTracker.LocalFiles["remove.txt"] = &synctracker.File{
		Name: "remove.txt",
	}

	require.NoError(t, kd.UnshareFile("remove.txt"))

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()

	_, ok := kd.SyncTracker.LocalFiles["keep.txt"]
	require.True(t, ok, "other files should not be affected")
	_, ok = kd.SyncTracker.LocalFiles["remove.txt"]
	require.False(t, ok, "removed file should be gone")
}

func TestUnshareFile_NoSessionNoPanic(t *testing.T) {
	kd := newUnshareTestKD()
	kd.SyncTracker.LocalFiles["file.txt"] = &synctracker.File{
		Name: "file.txt",
	}

	require.NoError(t, kd.UnshareFile("file.txt"), "UnshareFile without session should succeed")
}
