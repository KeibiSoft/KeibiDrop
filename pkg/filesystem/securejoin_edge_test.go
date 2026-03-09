// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecureJoin_SymlinkEscape verifies that SecureJoin rejects paths that escape
// via a symlink pointing outside the base directory (KD-SEC-2026-004).
func TestSecureJoin_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	base, err := os.MkdirTemp("", "keibidrop-symlink-base")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	outside, err := os.MkdirTemp("", "keibidrop-symlink-outside")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(outside); err != nil {
			t.Logf("cleanup: failed to remove outside dir: %v", err)
		}
	})

	// Symlink inside base → points to a directory outside base.
	symlinkPath := filepath.Join(base, "escape-link")
	require.NoError(t, os.Symlink(outside, symlinkPath))

	_, err = SecureJoin(base, "escape-link")
	assert.Error(t, err, "SecureJoin must reject a path that resolves via symlink to outside base")
}

// TestSecureJoin_SymlinkFileEscape verifies that a symlink to a file outside base
// is also rejected.
func TestSecureJoin_SymlinkFileEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	base, err := os.MkdirTemp("", "keibidrop-symlink-file-base")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	// Create a real file outside base.
	outsideFile, err := os.CreateTemp("", "keibidrop-outside-file")
	require.NoError(t, err)
	require.NoError(t, outsideFile.Close())
	t.Cleanup(func() {
		if err := os.Remove(outsideFile.Name()); err != nil {
			t.Logf("cleanup: failed to remove outside file: %v", err)
		}
	})

	symlinkPath := filepath.Join(base, "escape-file-link")
	require.NoError(t, os.Symlink(outsideFile.Name(), symlinkPath))

	_, err = SecureJoin(base, "escape-file-link")
	assert.Error(t, err, "SecureJoin must reject a path that symlinks to a file outside base")
}

// TestSecureJoin_SymlinkWithinBase verifies that a symlink pointing to a directory
// within base is accepted and resolves correctly.
func TestSecureJoin_SymlinkWithinBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevated privileges on Windows")
	}

	base, err := os.MkdirTemp("", "keibidrop-symlink-within")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	subdir := filepath.Join(base, "subdir")
	require.NoError(t, os.MkdirAll(subdir, 0o755))

	symlinkPath := filepath.Join(base, "safe-link")
	require.NoError(t, os.Symlink(subdir, symlinkPath))

	absBase, err := filepath.Abs(base)
	require.NoError(t, err)

	got, err := SecureJoin(base, "safe-link")
	assert.NoError(t, err, "SecureJoin must allow a symlink that resolves within base")
	assert.True(t, got == absBase || strings.HasPrefix(got, absBase+string(filepath.Separator)),
		"resolved path %q must be within %q", got, absBase)
}

// TestSecureJoin_EmptyAndDotPaths verifies that empty, dot, and dot-slash paths
// resolve to base (or within base) without escaping.
func TestSecureJoin_EmptyAndDotPaths(t *testing.T) {
	base, err := os.MkdirTemp("", "keibidrop-dot-paths")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	absBase, err := filepath.Abs(base)
	require.NoError(t, err)

	cases := []struct {
		name string
		path string
	}{
		{"empty string", ""},
		{"single dot", "."},
		{"dot-slash", "./"},
		{"dot-dot-dot", "././"},
		{"dot-slash-file", "./file.txt"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SecureJoin(base, tc.path)
			assert.NoError(t, err, "path %q should not escape base", tc.path)
			assert.True(t, got == absBase || strings.HasPrefix(got, absBase+string(filepath.Separator)),
				"path %q resolved to %q, expected within %q", tc.path, got, absBase)
		})
	}
}

// TestSecureJoin_UnicodeNormalization verifies that different Unicode normalization
// forms are treated as distinct paths on Linux (byte-exact comparison). This test
// only runs on Linux where the filesystem does not normalize filenames.
func TestSecureJoin_UnicodeNormalization(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Unicode normalization behaviour is OS-specific; this test targets Linux only")
	}

	base, err := os.MkdirTemp("", "keibidrop-unicode")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	absBase, err := filepath.Abs(base)
	require.NoError(t, err)

	// "café" NFC (é = U+00E9, one codepoint) vs NFD (e + U+0301, two codepoints).
	// On Linux both are valid, distinct filenames within base — neither escapes.
	nfc := "caf\u00e9.txt"           // precomposed
	nfd := "cafe\u0301.txt"          // decomposed
	combined := "caf\u00e9\u0301.txt" // unusual but valid

	for _, name := range []string{nfc, nfd, combined} {
		got, err := SecureJoin(base, name)
		assert.NoError(t, err, "Unicode filename %q should not escape base", name)
		assert.True(t, got == absBase || strings.HasPrefix(got, absBase+string(filepath.Separator)),
			"filename %q resolved to %q, expected within %q", name, got, absBase)
	}
}

// TestSecureJoin_CaseSensitiveLinux verifies that on Linux (case-sensitive FS),
// a path using a different case for the base directory name is treated as a distinct
// path and correctly rejected when it escapes.
func TestSecureJoin_CaseSensitiveLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Case-sensitivity test targets Linux only")
	}

	base, err := os.MkdirTemp("", "keibidrop-CaseTest")
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Logf("cleanup: failed to remove base dir: %v", err)
		}
	})

	// Attempt escape using upper-cased base directory name.
	upperBase := strings.ToUpper(filepath.Base(base))
	escapePath := "../" + upperBase + "/outside.txt"

	_, err = SecureJoin(base, escapePath)
	assert.Error(t, err, "Path %q should be rejected even if it only differs in case from base", escapePath)
}
