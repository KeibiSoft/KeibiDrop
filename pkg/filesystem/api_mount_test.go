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
