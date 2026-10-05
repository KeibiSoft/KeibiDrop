//go:build !android && !windows && !linux

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import "context"

// platformRefresh: no file manager refresh on this platform, so the mount
// starts no refresher. On macOS, Finder takes no AppleScript update on a
// macFUSE volume: it hangs, and the unmount then waits on it (measured 4 Oct:
// 117 s instead of 2.6 s).
func (fs *FS) platformRefresh(string) func(context.Context, map[string]PeerChange) {
	return nil
}
