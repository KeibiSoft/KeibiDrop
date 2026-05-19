// ABOUTME: Tests for UnshareFile — removes file from SyncTracker, errors on missing.
// ABOUTME: Covers the tracker mutation path; gRPC notification is fire-and-forget.
package common

import (
	"log/slog"
	"os"
	"testing"

	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
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

	if err := kd.UnshareFile("photo.jpg"); err != nil {
		t.Fatalf("UnshareFile: %v", err)
	}

	kd.SyncTracker.LocalFilesMu.RLock()
	_, exists := kd.SyncTracker.LocalFiles["photo.jpg"]
	kd.SyncTracker.LocalFilesMu.RUnlock()

	if exists {
		t.Fatal("file should be removed from LocalFiles")
	}
}

func TestUnshareFile_ErrorOnMissing(t *testing.T) {
	kd := newUnshareTestKD()

	err := kd.UnshareFile("nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for unsharing missing file")
	}
}

func TestUnshareFile_DoesNotAffectOtherFiles(t *testing.T) {
	kd := newUnshareTestKD()
	kd.SyncTracker.LocalFiles["keep.txt"] = &synctracker.File{
		Name: "keep.txt",
	}
	kd.SyncTracker.LocalFiles["remove.txt"] = &synctracker.File{
		Name: "remove.txt",
	}

	if err := kd.UnshareFile("remove.txt"); err != nil {
		t.Fatalf("UnshareFile: %v", err)
	}

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()

	if _, ok := kd.SyncTracker.LocalFiles["keep.txt"]; !ok {
		t.Fatal("other files should not be affected")
	}
	if _, ok := kd.SyncTracker.LocalFiles["remove.txt"]; ok {
		t.Fatal("removed file should be gone")
	}
}

func TestUnshareFile_NoSessionNoPanic(t *testing.T) {
	kd := newUnshareTestKD()
	kd.SyncTracker.LocalFiles["file.txt"] = &synctracker.File{
		Name: "file.txt",
	}

	if err := kd.UnshareFile("file.txt"); err != nil {
		t.Fatalf("UnshareFile without session should succeed: %v", err)
	}
}
