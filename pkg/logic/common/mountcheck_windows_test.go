// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

//go:build windows

package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The directory mount point WinFsp creates is a junction. A junction made
// by hand has the same reparse attribute, so it pins the detection; the
// plain directory it points at stays what it is.
func TestMountIsLive_WindowsJunctionIsAMountPoint(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(dir, "junction")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J not available: %v: %s", err, out)
	}
	if !MountIsLive(junction) {
		t.Fatalf("%s is a junction, the shape of a WinFsp directory mount point", junction)
	}
	if MountIsLive(target) {
		t.Fatalf("%s is an ordinary directory", target)
	}
}

// The system drive is a volume root, but its file system is NTFS, not FUSE:
// a drive letter counts as a live mount only when WinFsp owns it.
func TestMountIsLive_WindowsSystemDriveIsNotAMount(t *testing.T) {
	root := filepath.VolumeName(os.Getenv("SystemRoot")) + `\`
	if root == `\` {
		t.Skip("no SystemRoot")
	}
	if MountIsLive(root) {
		t.Fatalf("%s is a disk, not a WinFsp mount", root)
	}
}
