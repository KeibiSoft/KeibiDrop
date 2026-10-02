// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Regression test: a Read on a handle the map no longer holds (ClearFiles ran on a
// ABOUTME: disconnect while an app kept its fd) must take the cache fallback, not panic to EIO.
//go:build !android && !windows

package filesystem

import (
	"os"
	"path/filepath"
	"testing"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

func TestRead_UnknownHandleFallsBackToCache(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	if err := os.WriteFile(filepath.Join(saveDir, "kept.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 16)
	n := d.Read("/kept.txt", buf, 0, 4242) // 4242 is in no handle map
	if n == -winfuse.EIO {
		t.Fatalf("Read with an unknown handle returned EIO (panic recovered); want the cache fallback")
	}
	if n != 5 || string(buf[:n]) != "hello" {
		t.Fatalf("Read = %d %q, want 5 %q", n, buf[:max(n, 0)], "hello")
	}
}
