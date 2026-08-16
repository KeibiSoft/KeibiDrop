// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests for the mobile API package, covering the atomic snapshot pattern.
// ABOUTME: Uses direct SyncTracker population to avoid full KeibiDrop initialisation.

package mobile

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
)

// buildAPIWithTracker returns a minimal API wired to a fresh SyncTracker
// without starting the full KeibiDrop engine.  The kd.SyncTracker field is
// public so we can populate it directly.
func buildAPIWithTracker(t *testing.T) (*API, *synctracker.SyncTracker) {
	t.Helper()
	st := synctracker.NewSyncTracker()
	kd := &common.KeibiDrop{}
	kd.SyncTracker = st
	api := &API{kd: kd}
	return api, st
}

func TestRefreshFileList(t *testing.T) {
	api, st := buildAPIWithTracker(t)

	// Populate remote files. Names are intentionally unsorted.
	st.RemoteFiles["zebra.txt"] = &synctracker.File{Name: "zebra.txt", Size: 300}
	st.RemoteFiles["alpha.pdf"] = &synctracker.File{Name: "alpha.pdf", Size: 100}
	st.RemoteFiles["mango.jpg"] = &synctracker.File{Name: "mango.jpg", Size: 200}

	// Populate local files, also unsorted.
	st.LocalFiles["zulu.go"] = &synctracker.File{Name: "zulu.go", Size: 50}
	st.LocalFiles["bravo.rs"] = &synctracker.File{Name: "bravo.rs", Size: 75}
	st.LocalFiles["foxtrot.py"] = &synctracker.File{Name: "foxtrot.py", Size: 60}

	api.RefreshFileList()

	testkit.Run(t, func() error {
		return fp.All(
			// Remote snapshot. Names sort, so alpha.pdf is index 0.
			fp.Equal("GetRemoteFileCount()", api.GetRemoteFileCount(), 3),
			fp.Equal("GetRemoteFileName(0)", api.GetRemoteFileName(0), "alpha.pdf"),
			fp.Equal("GetRemoteFileSize(0)", api.GetRemoteFileSize(0), int64(100)),
			fp.Equal("GetRemoteFileName(2)", api.GetRemoteFileName(2), "zebra.txt"),
			fp.Equal("GetRemoteFileSize(2)", api.GetRemoteFileSize(2), int64(300)),

			// Local snapshot.
			fp.Equal("GetLocalFileCount()", api.GetLocalFileCount(), 3),
			fp.Equal("GetLocalFileName(0)", api.GetLocalFileName(0), "bravo.rs"),

			// Out-of-range index must not panic and must return the zero value.
			fp.Equal("GetRemoteFileName(99)", api.GetRemoteFileName(99), ""),
			fp.Equal("GetRemoteFileSize(99)", api.GetRemoteFileSize(99), int64(0)),
			fp.Equal("GetLocalFileName(99)", api.GetLocalFileName(99), ""),
		)
	})
}

func TestRefreshFileList_NilKd(t *testing.T) {
	api := &API{kd: nil}

	// Must not panic.
	api.RefreshFileList()

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("GetRemoteFileCount() with nil kd", api.GetRemoteFileCount(), 0),
			fp.Equal("GetLocalFileCount() with nil kd", api.GetLocalFileCount(), 0),
			fp.Equal("GetRemoteFileName(0) with nil kd", api.GetRemoteFileName(0), ""),
			fp.Equal("GetLocalFileName(0) with nil kd", api.GetLocalFileName(0), ""),
		)
	})
}
