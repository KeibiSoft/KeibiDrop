// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package service

import "testing"

// Trash folders and everything under them are internal, in both directions.
func TestIsInternalPath_OSTrash(t *testing.T) {
	for _, p := range []string{"/.Trashes", "/.Trashes/501/a.png", ".Trash-1000/files/x", "$RECYCLE.BIN/x", "/x.png.kdbitmap", "/.DS_Store"} {
		if !IsInternalPath(p) {
			t.Errorf("IsInternalPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"/a.png", "/docs/notes.txt", "/.git/config", "Trashes.txt"} {
		if IsInternalPath(p) {
			t.Errorf("IsInternalPath(%q) = true, want false", p)
		}
	}
}
