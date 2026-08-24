// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Fuzzes isUnsafeRemotePath for the one-directional containment
// ABOUTME: property: an accepted path must never resolve outside the root.

package service

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzIsUnsafeRemotePath asserts a one-directional property: whenever
// isUnsafeRemotePath accepts a path (returns false), joining it under a root
// with filepath.Join and filepath.Clean must not escape that root.
//
// The function deliberately rejects any literal ".." component without
// resolving the path, so a rejected input (e.g. "a/b/../c", which nets out
// safe once resolved) makes no claim either way. Only acceptance is checked.
func FuzzIsUnsafeRemotePath(f *testing.F) {
	seeds := []string{
		"../etc/passwd",
		"..\\windows\\system32",
		"a/../../b",
		"%2e%2e/x",
		"..foo", // legit: not the exact segment "..", must be accepted
		"a..b",  // legit: no ".." segment at all
		"/",
		"",
		"/normal/file.txt",
		"‥/etc/passwd",                           // U+2025 TWO DOT LEADER, not literal ".."
		"．．/windows/system32",                    // U+FF0E FULLWIDTH FULL STOP x2, not literal ".."
		strings.Repeat("../", 64) + "etc/passwd", // deeply nested chain
		"a\\b/../..\\c/d",                        // mixed separators
		" ",                                      // strips to nothing on Windows, resolves to the root
		"/. ",                                    // same, via a trailing dot-space component
		"foo ",                                   // aliases "foo" on a Windows disk
		"a/ /b",                                  // interior component strips to nothing
	}
	for _, s := range seeds {
		f.Add(s)
	}

	// Synthetic root: the oracle is pure string math (filepath.Join/Clean),
	// so this never needs to exist on disk.
	const root = "/save/root"
	sep := string(filepath.Separator)

	f.Fuzz(func(t *testing.T, p string) {
		if isUnsafeRemotePath(p) {
			return // rejected: the function makes no claim about the resolved path
		}
		joined := filepath.Clean(filepath.Join(root, p))
		if joined != root && !strings.HasPrefix(joined, root+sep) {
			t.Fatalf("accepted path %q escapes root: joined=%q root=%q", p, joined, root)
		}
		// Model the Windows per-component strip of trailing dots and spaces: even
		// after every component shrinks, an accepted path must stay contained and
		// must not land on the root itself.
		segs := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
		for i, seg := range segs {
			segs[i] = strings.TrimRight(seg, ". ")
		}
		winJoined := filepath.Clean(filepath.Join(root, strings.Join(segs, "/")))
		if winJoined == root || !strings.HasPrefix(winJoined, root+sep) {
			t.Fatalf("accepted path %q lands on or escapes the root after the Windows strip: %q", p, winJoined)
		}
	})
}
