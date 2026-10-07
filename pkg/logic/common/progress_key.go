// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Key-resolution helper for GetDownloadProgress — mirrors the fallback used by sibling FFI functions.
// ABOUTME: Handles FUSE-origin senders that register bitmap keys with a leading "/" vs bare callers.

package common

import "strings"

// resolveKeyWithFallback returns the canonical key and true when exists(key).
// It applies the same three-step precedence as KD_GetFileSizeByName and
// KD_SaveFileByName:
//
//  1. exact name
//  2. "/" + name  (bare query, slash-keyed map entry)
//  3. strings.TrimPrefix(name, "/")  (slash query, bare map entry)
//
// It returns ("", false) when no form matches.
func resolveKeyWithFallback(name string, exists func(string) bool) (string, bool) {
	if exists(name) {
		return name, true
	}
	if withSlash := "/" + name; exists(withSlash) {
		return withSlash, true
	}
	if stripped := strings.TrimPrefix(name, "/"); stripped != name && exists(stripped) {
		return stripped, true
	}
	return "", false
}
