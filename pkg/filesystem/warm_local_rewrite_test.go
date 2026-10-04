// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// A local rewrite that lands while a warm batch is in flight must win. The
// landing checked only the bitmap pointer, which a local truncate or write
// does not swap: it wrote the peer's old bytes over the user's fresh content,
// and the writer then announced them (TestFUSEtoFUSE_EditorSaves/
// OpenSCADRenderOverwrite: "cube([4,4," on the writer, 4 Oct).

package filesystem

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func TestWarmSiblings_LocalRewriteDuringTheBatchWins(t *testing.T) {
	files := map[string][]byte{
		"/evd/a.bin":      makePattern(8 * 1024),
		"/evd/model.scad": []byte("cube([4,4,4]);"),
	}
	p := &warmBatchProvider{
		files:   files,
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	root := newWarmRoot(t, p, plainStat)

	root.maybeWarmSiblings("/evd/a.bin")
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never started")
	}

	// While the batch is in flight the user rewrites model.scad through the
	// mount, as os.WriteFile does on Linux: open, truncate, write, close.
	var st winfuse.Stat_t
	require.Equal(t, 0, root.Getattr("/evd/model.scad", &st, ^uint64(0)))
	fi := &winfuse.FileInfo_t{Flags: os.O_WRONLY}
	require.Equal(t, 0, root.OpenEx("/evd/model.scad", fi))
	require.Equal(t, 0, root.Truncate("/evd/model.scad", 0, fi.Fh))
	require.Equal(t, 10, root.Write("/evd/model.scad", []byte("sphere(3);"), 0, fi.Fh))
	require.Equal(t, 0, root.Release("/evd/model.scad", fi.Fh))

	close(p.release)
	testkit.Eventually(t, 5*time.Second, 10*time.Millisecond, warmSettled(root, "/evd"), "the warm batch to settle")
	got, err := os.ReadFile(filepath.Join(root.LocalDownloadFolder, "evd", "model.scad"))
	require.NoError(t, err)
	require.Equal(t, "sphere(3);", string(got), "the warm landing must not overwrite the local rewrite")
}

// startGatedWarm triggers a warm of /evd from a.bin and returns once the batch
// is in flight; close(p.release) lets the frames land.
func startGatedWarm(t *testing.T, files map[string][]byte) (*Dir, *warmBatchProvider) {
	t.Helper()
	p := &warmBatchProvider{
		files:   files,
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	root := newWarmRoot(t, p, plainStat)
	root.maybeWarmSiblings("/evd/a.bin")
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("batch never started")
	}
	return root, p
}

// A file deleted while its warm batch is in flight stays deleted: the landing
// opens with O_CREATE and would have written the peer's bytes back.
func TestWarmSiblings_LocalDeleteDuringTheBatchStaysDeleted(t *testing.T) {
	root, p := startGatedWarm(t, map[string][]byte{
		"/evd/a.bin":    makePattern(8 * 1024),
		"/evd/gone.txt": []byte("the peer's bytes"),
	})
	var st winfuse.Stat_t
	require.Equal(t, 0, root.Getattr("/evd/gone.txt", &st, ^uint64(0)))
	require.Equal(t, 0, root.Unlink("/evd/gone.txt"))

	close(p.release)
	testkit.Eventually(t, 5*time.Second, 10*time.Millisecond, warmSettled(root, "/evd"), "the warm batch to settle")
	_, err := os.Stat(filepath.Join(root.LocalDownloadFolder, "evd", "gone.txt"))
	require.True(t, os.IsNotExist(err), "the warm landing must not bring a deleted file back")
}

// An editor's save (temp file, rename over the target) while the target's warm
// batch is in flight keeps the saved bytes.
func TestWarmSiblings_RenameOverDuringTheBatchKeepsTheSave(t *testing.T) {
	root, p := startGatedWarm(t, map[string][]byte{
		"/evd/a.bin":   makePattern(8 * 1024),
		"/evd/doc.txt": []byte("the peer's old version"),
	})
	fi := &winfuse.FileInfo_t{}
	require.Equal(t, 0, root.CreateEx("/evd/doc.txt.tmp", 0o644, fi))
	require.Equal(t, 9, root.Write("/evd/doc.txt.tmp", []byte("new local"), 0, fi.Fh))
	require.Equal(t, 0, root.Release("/evd/doc.txt.tmp", fi.Fh))
	require.Equal(t, 0, root.Rename("/evd/doc.txt.tmp", "/evd/doc.txt"))

	close(p.release)
	testkit.Eventually(t, 5*time.Second, 10*time.Millisecond, warmSettled(root, "/evd"), "the warm batch to settle")
	got, err := os.ReadFile(filepath.Join(root.LocalDownloadFolder, "evd", "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, "new local", string(got), "the warm landing must not overwrite the saved file")
}
