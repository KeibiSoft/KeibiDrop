// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package types

import "testing"

func TestIsOSTrashPath(t *testing.T) {
	yes := []string{"/.Trashes", ".Trashes", "/.Trashes/501/a.png", ".Trash-1000/files/x", "/.Trash-501", "$RECYCLE.BIN/S-1-5-21/x", `\.Trashes\501\a.png`}
	no := []string{"/a.png", "/docs/.Trashes/x", "Trashes.txt", "my.Trash-notes.txt", "/.Trashed", "/.kdbitmap", ""}
	for _, p := range yes {
		if !IsOSTrashPath(p) {
			t.Errorf("IsOSTrashPath(%q) = false, want true", p)
		}
	}
	for _, p := range no {
		if IsOSTrashPath(p) {
			t.Errorf("IsOSTrashPath(%q) = true, want false", p)
		}
	}
}
