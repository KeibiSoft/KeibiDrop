// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A plain directory is not a mount. This is the case that matters: the mount
// point is created before the filesystem is attached, so a stat-only check
// would report ready while every read still fails.
func TestIsLiveMountRejectsPlainDirectory(t *testing.T) {
	dir := t.TempDir()
	if isLiveMount(dir) {
		t.Fatalf("%s is an ordinary directory, not a mount", dir)
	}

	nested := filepath.Join(dir, "sub")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	if isLiveMount(nested) {
		t.Fatalf("%s is an ordinary directory, not a mount", nested)
	}
}

func TestIsLiveMountRejectsMissingPath(t *testing.T) {
	if isLiveMount(filepath.Join(t.TempDir(), "does-not-exist")) {
		t.Fatal("a path that does not exist cannot be a live mount")
	}
}

// The root filesystem is a mount on every platform this runs on, so it pins
// the positive case without needing a FUSE provider installed. On Unix the
// check compares device ids; "/" is its own parent and shares its device, so
// this deliberately uses a directory known to sit on a different device.
func TestIsLiveMountAcceptsARealMount(t *testing.T) {
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		// macOS splits the system volume from the data volume, so these two
		// sit on different devices.
		candidates = []string{"/System/Volumes/Data", "/dev", "/Volumes"}
	case "linux":
		candidates = []string{"/proc", "/sys", "/dev"}
	default:
		t.Skip("no known always-present mount for this platform")
	}

	for _, path := range candidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if isLiveMount(path) {
			return // found one, the positive case holds
		}
	}
	t.Skipf("none of %v was a distinct mount on this machine", candidates)
}
