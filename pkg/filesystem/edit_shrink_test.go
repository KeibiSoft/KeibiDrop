//go:build !android

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// A peer's EDIT_FILE that makes the file smaller left the old bytes past the
// new end in the cache file. The fetch wrote the new bytes over the old ones
// and reads served both: a 10-byte "sphere(3);" read back as "sphere(3);4]);"
// (TestFUSEtoFUSE_EditorSaves/OpenSCADRenderOverwrite, 4 Oct).

package filesystem

import (
	"os"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func TestEditRemoteFile_ShrinkDropsTheOldTail(t *testing.T) {
	d, _ := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/model.scad", "cube([4,4,4]);")
	const newSize = 10
	want := make([]byte, newSize)
	for i := range want {
		want[i] = fillByte(int64(i))
	}
	prov := &fillProvider{size: newSize}
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })

	// The peer rewrote the file in place: smaller, newer, sent as EDIT_FILE.
	require.NoError(t, d.EditRemoteFileWithBase(d.logger, "/model.scad", "model.scad",
		remoteStat(newSize, time.Now().Add(time.Second)), 0))
	info, err := os.Stat(f.RealPathOfFile)
	require.NoError(t, err)
	require.Equal(t, int64(newSize), info.Size(), "the cache file must not keep the old tail")

	// What the reader sees through the mount, before and after the fill.
	for round := 1; round <= 2; round++ {
		var st winfuse.Stat_t
		require.Equal(t, 0, d.Getattr("/model.scad", &st, ^uint64(0)))
		require.Equalf(t, int64(newSize), st.Size, "round %d: Getattr size", round)
		fi := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
		require.Equal(t, 0, d.OpenEx("/model.scad", fi))
		buf := make([]byte, 64)
		n := d.Read("/model.scad", buf, 0, fi.Fh)
		require.Equal(t, 0, d.Release("/model.scad", fi.Fh))
		require.Equalf(t, want, buf[:max(n, 0)], "round %d: a read must serve the new version only", round)
	}
}
