//go:build !android

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"context"
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PeerChange is what a peer did to a path in the mount.
type PeerChange uint8

const (
	PeerAdded PeerChange = iota + 1
	PeerEdited
	PeerRemoved
	PeerDirAdded
	PeerDirRemoved
)

// A file manager does not see changes the daemon makes for a peer (no kernel
// event fires), so an open Explorer or Nautilus window stays stale. The
// refresher tells it, per platform (refresh_<os>.go). It flushes from its
// own goroutine, never inside a FUSE handler, at most once per interval and
// at most maxRefreshBatch paths per flush: a burst of announces is one refresh.
const (
	refreshInterval = 2 * time.Second
	maxRefreshBatch = 512
	maxRefreshDirs  = 32
	maxKnownDirs    = 4096
	refreshStopWait = 2 * time.Second // Windows unmount: bound on one notify
)

type fileManagerRefresher struct {
	mu       sync.Mutex
	pending  map[string]PeerChange // FUSE path ("/a/b") -> latest change
	known    map[string]struct{}   // folders made on the way and already named
	kick     chan struct{}
	done     chan struct{} // closed when run returns
	interval time.Duration
	flush    func(ctx context.Context, batch map[string]PeerChange)
	logger   *slog.Logger // nil in tests
}

func newFileManagerRefresher(flush func(context.Context, map[string]PeerChange)) *fileManagerRefresher {
	return &fileManagerRefresher{
		pending:  make(map[string]PeerChange),
		known:    make(map[string]struct{}),
		kick:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		interval: refreshInterval,
		flush:    flush,
	}
}

func (r *fileManagerRefresher) add(p string, c PeerChange) {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	r.mu.Lock()
	r.pending[p] = c
	switch c {
	case PeerAdded, PeerDirAdded:
		// A peer's deep file makes its folders on the way (no ADD_DIR), so the
		// window on each parent learns about them too: once, not per file.
		if len(r.known) >= maxKnownDirs {
			clear(r.known) // a folder named again costs one refresh, not memory
		}
		for d := path.Dir(p); d != "/"; d = path.Dir(d) {
			if _, ok := r.known[d]; ok {
				break // its own parents were named with it
			}
			r.known[d] = struct{}{}
			if prev, ok := r.pending[d]; !ok || prev == PeerDirRemoved {
				r.pending[d] = PeerDirAdded
			}
		}
	case PeerDirRemoved:
		for d := range r.known {
			if d == p || strings.HasPrefix(d, p+"/") {
				delete(r.known, d)
			}
		}
	}
	r.mu.Unlock()
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

func (r *fileManagerRefresher) run(ctx context.Context) {
	defer close(r.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(r.interval):
		}
		r.mu.Lock()
		batch := make(map[string]PeerChange, min(len(r.pending), maxRefreshBatch))
		for p, c := range r.pending {
			if len(batch) == maxRefreshBatch {
				break
			}
			batch[p] = c
			delete(r.pending, p)
		}
		more := len(r.pending) > 0
		r.mu.Unlock()
		if len(batch) > 0 {
			if r.logger != nil {
				r.logger.Debug("File manager refresh", "paths", len(batch), "more", more)
			}
			r.flush(ctx, batch)
		}
		if more {
			select {
			case r.kick <- struct{}{}:
			default:
			}
		}
	}
}

// PeerChanged queues a peer change in the mount for the file manager refresh.
// It never blocks; without a mount it does nothing.
func (fs *FS) PeerChanged(p string, c PeerChange) {
	if r := fs.refresher.Load(); r != nil {
		r.add(p, c)
	}
}

// changedDirs turns a batch into the absolute folders a window shows: the
// parent of each changed path. At most maxRefreshDirs, sorted.
func changedDirs(mountPoint string, batch map[string]PeerChange) []string {
	seen := make(map[string]struct{})
	for p := range batch {
		seen[path.Dir(p)] = struct{}{}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, filepath.Join(mountPoint, filepath.FromSlash(d)))
	}
	sort.Strings(dirs)
	if len(dirs) > maxRefreshDirs {
		dirs = dirs[:maxRefreshDirs]
	}
	return dirs
}
