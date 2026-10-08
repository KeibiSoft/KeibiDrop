// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package types

import "strings"

// IsOSTrashPath reports whether path is under a file manager's trash folder at the
// mount root (.Trashes, .Trash-<uid>, $RECYCLE.BIN). Such paths never sync.
func IsOSTrashPath(path string) bool {
	p := strings.TrimLeft(strings.ReplaceAll(path, "\\", "/"), "/")
	first := p
	if i := strings.IndexByte(p, '/'); i >= 0 {
		first = p[:i]
	}
	return first == ".Trashes" || strings.HasPrefix(first, ".Trash-") || first == "$RECYCLE.BIN"
}
