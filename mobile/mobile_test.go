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

	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
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

	// Populate remote files — intentionally unsorted names.
	st.RemoteFiles["zebra.txt"] = &synctracker.File{Name: "zebra.txt", Size: 300}
	st.RemoteFiles["alpha.pdf"] = &synctracker.File{Name: "alpha.pdf", Size: 100}
	st.RemoteFiles["mango.jpg"] = &synctracker.File{Name: "mango.jpg", Size: 200}

	// Populate local files — also unsorted.
	st.LocalFiles["zulu.go"] = &synctracker.File{Name: "zulu.go", Size: 50}
	st.LocalFiles["bravo.rs"] = &synctracker.File{Name: "bravo.rs", Size: 75}
	st.LocalFiles["foxtrot.py"] = &synctracker.File{Name: "foxtrot.py", Size: 60}

	api.RefreshFileList()

	// --- Remote snapshot assertions ---
	if got := api.GetRemoteFileCount(); got != 3 {
		t.Errorf("GetRemoteFileCount() = %d, want 3", got)
	}
	if got := api.GetRemoteFileName(0); got != "alpha.pdf" {
		t.Errorf("GetRemoteFileName(0) = %q, want %q", got, "alpha.pdf")
	}
	if got := api.GetRemoteFileSize(0); got != 100 {
		t.Errorf("GetRemoteFileSize(0) = %d, want 100", got)
	}
	if got := api.GetRemoteFileName(2); got != "zebra.txt" {
		t.Errorf("GetRemoteFileName(2) = %q, want %q", got, "zebra.txt")
	}
	if got := api.GetRemoteFileSize(2); got != 300 {
		t.Errorf("GetRemoteFileSize(2) = %d, want 300", got)
	}

	// --- Local snapshot assertions ---
	if got := api.GetLocalFileCount(); got != 3 {
		t.Errorf("GetLocalFileCount() = %d, want 3", got)
	}
	if got := api.GetLocalFileName(0); got != "bravo.rs" {
		t.Errorf("GetLocalFileName(0) = %q, want %q", got, "bravo.rs")
	}

	// --- Bounds-safety ---
	if got := api.GetRemoteFileName(99); got != "" {
		t.Errorf("GetRemoteFileName(99) = %q, want empty string", got)
	}
	if got := api.GetRemoteFileSize(99); got != 0 {
		t.Errorf("GetRemoteFileSize(99) = %d, want 0", got)
	}
	if got := api.GetLocalFileName(99); got != "" {
		t.Errorf("GetLocalFileName(99) = %q, want empty string", got)
	}
}

func TestRefreshFileList_NilKd(t *testing.T) {
	api := &API{kd: nil}

	// Must not panic.
	api.RefreshFileList()

	if got := api.GetRemoteFileCount(); got != 0 {
		t.Errorf("GetRemoteFileCount() with nil kd = %d, want 0", got)
	}
	if got := api.GetLocalFileCount(); got != 0 {
		t.Errorf("GetLocalFileCount() with nil kd = %d, want 0", got)
	}
	if got := api.GetRemoteFileName(0); got != "" {
		t.Errorf("GetRemoteFileName(0) with nil kd = %q, want empty string", got)
	}
	if got := api.GetLocalFileName(0); got != "" {
		t.Errorf("GetLocalFileName(0) with nil kd = %q, want empty string", got)
	}
}
