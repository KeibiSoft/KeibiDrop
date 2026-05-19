// ABOUTME: Tests for PruneStaleLocalFiles — removes entries for deleted files.
// ABOUTME: Covers existing files, deleted files, empty paths, and empty tracker.
package synctracker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPruneStaleLocalFiles_RemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	if err := os.WriteFile(existing, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}

	st := NewSyncTracker()
	st.LocalFiles["exists.txt"] = &File{
		Name:           "exists.txt",
		RealPathOfFile: existing,
	}
	st.LocalFiles["gone.txt"] = &File{
		Name:           "gone.txt",
		RealPathOfFile: filepath.Join(dir, "gone.txt"),
	}

	st.PruneStaleLocalFiles()

	if _, ok := st.LocalFiles["exists.txt"]; !ok {
		t.Fatal("existing file should be kept")
	}
	if _, ok := st.LocalFiles["gone.txt"]; ok {
		t.Fatal("deleted file should be pruned")
	}
}

func TestPruneStaleLocalFiles_SkipsEmptyPath(t *testing.T) {
	st := NewSyncTracker()
	st.LocalFiles["no-path"] = &File{
		Name:           "no-path",
		RealPathOfFile: "",
	}

	st.PruneStaleLocalFiles()

	if _, ok := st.LocalFiles["no-path"]; !ok {
		t.Fatal("entry with empty RealPathOfFile should be kept")
	}
}

func TestPruneStaleLocalFiles_EmptyTracker(t *testing.T) {
	st := NewSyncTracker()
	st.PruneStaleLocalFiles()

	if len(st.LocalFiles) != 0 {
		t.Fatal("empty tracker should remain empty")
	}
}

func TestPruneStaleLocalFiles_AllDeleted(t *testing.T) {
	st := NewSyncTracker()
	st.LocalFiles["a.txt"] = &File{
		Name:           "a.txt",
		RealPathOfFile: "/nonexistent/a.txt",
	}
	st.LocalFiles["b.txt"] = &File{
		Name:           "b.txt",
		RealPathOfFile: "/nonexistent/b.txt",
	}

	st.PruneStaleLocalFiles()

	if len(st.LocalFiles) != 0 {
		t.Fatalf("all stale files should be pruned, got %d", len(st.LocalFiles))
	}
}
