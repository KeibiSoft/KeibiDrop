// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Rejects a peer-supplied NotifyRequest path that could escape the
// ABOUTME: save root. No build tag: both Notify build variants share it.

package service

import (
	"runtime"
	"strings"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

// isUnsafeRemotePath reports whether a peer-supplied path could write outside the
// save root, or onto the root itself. KeibiDrop paths are mount-absolute and
// filepath.Join keeps a leading separator under the save root, so the ways out are
// a ".." component, a component Windows normalizes into one or strips to nothing,
// a Windows alias of a sibling name, or a path that names no entry at all and so
// resolves to the root. Split on both "/" and "\" before checking components,
// since a Windows peer sends "\".
//
// This validates the raw peer path at the ingest boundary, uniformly on both
// builds. It is the sibling of filesystem.isInsideRoot, which does resolved-path
// containment at the FUSE mkdir sites; the service disk sinks have no such guard.
func isUnsafeRemotePath(p string) bool {
	if p == "" {
		return true
	}
	named := 0
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == "." {
			continue // current-dir hop, never a name
		}
		if isDotDotComponent(seg) {
			return true
		}
		// Windows strips trailing dots and spaces from every component. One that
		// strips to nothing resolves to its parent, which can be the save root, so
		// those forms are rejected on every build. One that merely shrinks ("foo ")
		// aliases a sibling name, which only a Windows disk can reach: reject it
		// there and let POSIX keep such names.
		trimmed := strings.TrimRight(seg, ". ")
		if trimmed == "" {
			return true
		}
		if runtime.GOOS == "windows" && trimmed != seg {
			return true
		}
		named++
	}
	// "/", "." and friends resolve to the save root itself. That is never a legitimate
	// target: the rename branch would drive os.Remove or RenameShared on the root.
	return named == 0
}

// isUnsafeNotify reports whether a peer notification carries a path that could escape
// the save root or target it directly. OldPath is only meaningful for a rename, where
// it must be present and safe; every other type legitimately leaves it empty.
func isUnsafeNotify(t bindings.NotifyType, path, oldPath string) bool {
	if isUnsafeRemotePath(path) {
		return true
	}
	if t == bindings.NotifyType_RENAME_FILE || t == bindings.NotifyType_RENAME_DIR {
		return isUnsafeRemotePath(oldPath)
	}
	return oldPath != "" && isUnsafeRemotePath(oldPath)
}

// isDotDotComponent reports whether a component reaches the filesystem as "..".
// Windows strips trailing spaces and dots from every component, so ".. " and
// "..." also land as a parent hop. A name holding anything else, like
// "..bashrc", is an ordinary file and must still pass.
func isDotDotComponent(seg string) bool {
	dots := 0
	for _, r := range seg {
		switch r {
		case '.':
			dots++
		case ' ':
		default:
			return false
		}
	}
	return dots >= 2
}
