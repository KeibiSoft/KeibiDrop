// ABOUTME: Tests for PruneStaleLocalFiles, which removes entries for deleted files.
// ABOUTME: Covers existing files, deleted files, empty paths, and empty tracker.
package synctracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

func TestPruneStaleLocalFiles_RemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.txt")
	require.NoError(t, os.WriteFile(existing, []byte("data"), 0600))

	st := NewSyncTracker()
	st.LocalFiles["exists.txt"] = &File{Name: "exists.txt", RealPathOfFile: existing}
	st.LocalFiles["gone.txt"] = &File{Name: "gone.txt", RealPathOfFile: filepath.Join(dir, "gone.txt")}

	st.PruneStaleLocalFiles()

	_, keptExisting := st.LocalFiles["exists.txt"]
	_, keptGone := st.LocalFiles["gone.txt"]

	testkit.Run(t, func() error {
		return fp.All(
			fp.True("existing file kept", keptExisting),
			fp.False("deleted file pruned", keptGone),
		)
	})
}

func TestPruneStaleLocalFiles_SkipsEmptyPath(t *testing.T) {
	st := NewSyncTracker()
	st.LocalFiles["no-path"] = &File{
		Name:           "no-path",
		RealPathOfFile: "",
	}

	st.PruneStaleLocalFiles()

	_, kept := st.LocalFiles["no-path"]
	require.True(t, kept, "entry with empty RealPathOfFile must be kept")
}

func TestPruneStaleLocalFiles_EmptyTracker(t *testing.T) {
	st := NewSyncTracker()
	st.PruneStaleLocalFiles()

	require.Len(t, st.LocalFiles, 0, "empty tracker must remain empty")
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

	require.Len(t, st.LocalFiles, 0, "all stale files should be pruned")
}

// The count moves with every write and every new tracker, never with a
// read: the app's file list polls it without a lock.
func TestListWritesFollowsWritesAndNewTrackers(t *testing.T) {
	st := NewSyncTracker()
	w0 := ListWrites()

	st.LocalFilesMu.RLock()
	st.LocalFilesMu.RUnlock()
	if ListWrites() != w0 {
		t.Fatal("a read moved the count")
	}

	st.RemoteFilesMu.Lock()
	st.RemoteFiles["a"] = &File{Name: "a"}
	st.RemoteFilesMu.Unlock()
	w1 := ListWrites()
	if w1 <= w0 {
		t.Fatal("a remote write did not move the count")
	}

	st.LocalFilesMu.Lock()
	st.LocalFilesMu.Unlock()
	if ListWrites() <= w1 {
		t.Fatal("a local write did not move the count")
	}

	w2 := ListWrites()
	NewSyncTracker()
	if ListWrites() <= w2 {
		t.Fatal("a new tracker did not move the count")
	}
}
