//go:build !android && !windows && !linux

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import "context"

// platformRefresh: no file manager refresh on this platform, so the mount
// starts no refresher. On macOS, Finder takes no AppleScript update on a
// macFUSE volume: it hangs, and the unmount then waits on it (measured 4 Oct:
// 117 s instead of 2.6 s).
func (fs *FS) platformRefresh(string) func(context.Context, map[string]PeerChange) {
	return nil
}
