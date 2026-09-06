// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Announces that arrive before the mount wait in the tracker; the fold-in
// ABOUTME: must put them in the tree, new folders included, once the root exists.

package common

import (
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// A laptop restart loses the tree; the box re-announces its files in the first
// seconds of the session, before the mount is up, so they land in the tracker
// only. The fold-in ran before the root existed and dropped them: the mount
// then showed only files with a cache copy on disk (2026-09-06, bench/ empty,
// rand-300m.bin gone). Mount now fires the fold-in once the root exists.
func TestBackfill_FoldsPreMountAnnouncesIntoFreshRoot(t *testing.T) {
	kd := newBareKD()
	kd.SyncTracker = synctracker.NewSyncTracker()
	kd.SyncTracker.RemoteFiles["newdir/f.bin"] = &synctracker.File{
		Name: "f.bin", RelativePath: "newdir/f.bin", Size: 1024,
		LastEditTime: uint64(time.Now().UnixNano()),
	}
	kd.SyncTracker.RemoteFiles["top.bin"] = &synctracker.File{
		Name: "top.bin", RelativePath: "top.bin", Size: 2048,
		LastEditTime: uint64(time.Now().UnixNano()),
	}
	fs := filesystem.NewFS(kd.logger)
	fs.OnRootReady = kd.BackfillRemoteFilesIntoFS
	kd.FS = fs

	// Before the root exists the fold-in is a no-op, not a crash.
	kd.BackfillRemoteFilesIntoFS()

	root := filesystem.NewBareRoot(t.TempDir())
	fs.SetRoot(root)
	fs.OnRootReady() // what Mount does right after storing the root

	var st winfuse.Stat_t
	require.Equal(t, 0, root.Getattr("/newdir", &st, ^uint64(0)), "the new folder must exist in the tree")
	require.Equal(t, 0, root.Getattr("/newdir/f.bin", &st, ^uint64(0)))
	require.EqualValues(t, 1024, st.Size)
	require.Equal(t, 0, root.Getattr("/top.bin", &st, ^uint64(0)))
	require.EqualValues(t, 2048, st.Size)
}
