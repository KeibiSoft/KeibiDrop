// ABOUTME: Tests for FS.Mount error handling — verifies invalid mount points return error.
// ABOUTME: The host.Mount failure branch is covered by integration tests, not here.

package filesystem

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func newTestFS() *FS {
	return NewFS(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestMount_EmptyMountPoint_ReturnsError(t *testing.T) {
	err := newTestFS().Mount("", false, "/tmp/x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid mount point") {
		t.Fatalf("expected 'invalid mount point' error, got: %v", err)
	}
}

func TestMount_DotMountPoint_ReturnsError(t *testing.T) {
	err := newTestFS().Mount(".", false, "/tmp/x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestMount_TrailingSlashCleansToDot_ReturnsError(t *testing.T) {
	// filepath.Clean("./") == "."
	err := newTestFS().Mount("./", false, "/tmp/x")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// cgofuse panics instead of returning false when it cannot load the FUSE
// library. The panic must become an error, not take the process down.
func TestHostMount_PanicBecomesError(t *testing.T) {
	ok, err := hostMount(func() bool { panic("cgofuse: cannot find FUSE") })
	if ok {
		t.Fatal("expected ok false after a panic")
	}
	if err == nil || !strings.Contains(err.Error(), "cannot find FUSE") {
		t.Fatalf("expected the panic text as the error, got: %v", err)
	}
}

func TestHostMount_PassesResultThrough(t *testing.T) {
	for _, want := range []bool{true, false} {
		ok, err := hostMount(func() bool { return want })
		if err != nil || ok != want {
			t.Fatalf("hostMount(-> %v) = %v, %v", want, ok, err)
		}
	}
}

func TestIsWindowsDirMountPoint(t *testing.T) {
	cases := []struct {
		goos, p string
		want    bool
	}{
		{"windows", `C:\kd\mnt`, true}, // directory mount point: must be absent for WinFSP
		{"windows", "K:", false},       // bare drive letter: leave alone
		{"windows", "Z:", false},
		{"linux", "/home/u/mnt", false}, // non-Windows: not handled here
		{"darwin", "/Users/u/mnt", false},
	}
	for _, c := range cases {
		if got := isWindowsDirMountPoint(c.goos, c.p); got != c.want {
			t.Errorf("isWindowsDirMountPoint(%q, %q) = %v, want %v", c.goos, c.p, got, c.want)
		}
	}
}
