//go:build windows

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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
