// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
