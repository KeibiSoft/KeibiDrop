// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: skipPeerSync, the FUSE notify worker's filter: macOS metadata stays local, including the files
// ABOUTME: fseventsd writes inside its .fseventsd folder; everything else, .fuse_hidden renames included, is sent.

package common

import "testing"

func TestSkipPeerSync(t *testing.T) {
	for path, want := range map[string]bool{
		"/.DS_Store":                   true,
		"/photos/.DS_Store":            true,
		"/._clip.mov":                  true,
		"/photos/._a.jpg":              true,
		"/.fseventsd":                  true,
		"/.fseventsd/fseventsd-uuid":   true, // macOS writes it into every mounted volume
		"/.fseventsd/00000000004cb2a3": true,
		"/report.txt":                  false,
		"/photos/a.jpg":                false,
		"/notes.fseventsd.txt":         false,
		"/my.fseventsd/a.txt":          false,
		// The receiver turns a rename to .fuse_hiddenN into a remove of the source: it must be sent.
		"/.fuse_hidden000012a400000001": false,
	} {
		if got := skipPeerSync(path); got != want {
			t.Errorf("skipPeerSync(%q) = %v, want %v", path, got, want)
		}
	}
}
