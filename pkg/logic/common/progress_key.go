// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Key-resolution helper for GetDownloadProgress — mirrors the fallback used by sibling FFI functions.
// ABOUTME: Handles FUSE-origin senders that register bitmap keys with a leading "/" vs bare callers.

package common

import "strings"

// resolveKeyWithFallback returns the canonical key and true when exists(key)
// using the same three-step precedence that KD_GetFileSizeByName and
// KD_SaveFileByName apply:
//
//  1. exact name
//  2. "/" + name  (bare query, slash-keyed map entry)
//  3. strings.TrimPrefix(name, "/")  (slash query, bare map entry)
//
// An exact match always wins; the two fallbacks are only tried in order when
// the exact key is absent.  Returns ("", false) when none of the three forms
// is present.
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
