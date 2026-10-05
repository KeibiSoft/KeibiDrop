//go:build windows

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

// platformRefresh: WinFsp tells Explorer (and any other directory watcher)
// through its change notification. Each call takes WinFsp's rename lock, so
// it runs only from the refresher goroutine, batched. Destroy cancels ctx and
// waits for this loop, so it stops after the notify in progress.
func (fs *FS) platformRefresh(string) func(context.Context, map[string]PeerChange) {
	return func(ctx context.Context, batch map[string]PeerChange) {
		host := fs.host.Load()
		if host == nil {
			return
		}
		for p, c := range batch {
			if ctx.Err() != nil {
				return
			}
			host.Notify(p, notifyAction(c))
		}
	}
}

func notifyAction(c PeerChange) uint32 {
	switch c {
	case PeerAdded:
		return winfuse.NOTIFY_CREATE
	case PeerRemoved:
		return winfuse.NOTIFY_UNLINK
	case PeerDirAdded:
		return winfuse.NOTIFY_MKDIR
	case PeerDirRemoved:
		return winfuse.NOTIFY_RMDIR
	default:
		return winfuse.NOTIFY_TRUNCATE | winfuse.NOTIFY_UTIME
	}
}
