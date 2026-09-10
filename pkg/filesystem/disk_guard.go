// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Free-space guard for the save folder: no fetch starts under the floor,
// ABOUTME: so a landing never runs the disk to zero mid-write.

package filesystem

import (
	"sync"
	"sync/atomic"
	"time"
)

// LowDiskFloor is the free space on the save folder's disk under which no
// fetch starts: two read-ahead blocks in flight plus a margin for the
// sidecars and the OS. A read that needs bytes from the peer then fails with
// ENOSPC instead of leaving a half-written cache file on a full disk.
const LowDiskFloor = 2*ReadAheadBlock + 32<<20

// lowDiskClear is the free space at which the guard opens again: one block
// above the floor, so a disk hovering at the floor does not flap.
const lowDiskClear = LowDiskFloor + ReadAheadBlock

// diskGuardTTL paces the statfs behind the guard: a fetch every 2 MiB must
// not ask the OS every time.
const diskGuardTTL = time.Second

// freeDiskSpace is what the guard asks the OS; tests replace it.
var freeDiskSpace = func(path string) (uint64, error) {
	avail, _, _, err := GetFreeDiskSpace(path)
	return avail, err
}

// diskGuard remembers the last free-space answer and whether the disk is
// under the floor. Root-only, like the callbacks.
type diskGuard struct {
	mu      sync.Mutex
	checked time.Time
	free    uint64
	low     atomic.Bool
	onLow   atomic.Pointer[func(low bool, free uint64)]
}

// SetOnLowDisk publishes the low-disk report, called once per crossing in
// each direction. Call it on the root.
func (d *Dir) SetOnLowDisk(fn func(low bool, free uint64)) {
	d.disk.onLow.Store(&fn)
}

// LowDisk reports whether the last check found the save folder's disk under
// the floor.
func (d *Dir) LowDisk() bool {
	return d.guardRoot().disk.low.Load()
}

func (d *Dir) guardRoot() *Dir {
	if d.Root != nil {
		return d.Root
	}
	return d
}

// fetchAllowed reports whether a fetch may start. It asks the OS at most once
// per diskGuardTTL, logs once per crossing and reports the crossing through
// SetOnLowDisk. A statfs error allows the fetch: the OS says ENOSPC itself
// when it comes to that, and a guard that failed closed would stop every read
// over a disk that does not answer statfs.
func (d *Dir) fetchAllowed() bool {
	r := d.guardRoot()
	g := &r.disk
	g.mu.Lock()
	var crossed *bool
	if time.Since(g.checked) >= diskGuardTTL {
		g.checked = time.Now()
		if free, err := freeDiskSpace(r.LocalDownloadFolder); err == nil {
			g.free = free
			was := g.low.Load()
			low := was
			switch {
			case free < LowDiskFloor:
				low = true
			case free >= lowDiskClear:
				low = false
			}
			if low != was {
				g.low.Store(low)
				crossed = &low
			}
		}
	}
	low, free := g.low.Load(), g.free
	g.mu.Unlock()
	if crossed != nil {
		if low {
			r.logger.Warn("Save folder disk is almost full, fetches stop", "free_mb", free>>20, "floor_mb", LowDiskFloor>>20)
		} else {
			r.logger.Info("Save folder disk has space again, fetches resume", "free_mb", free>>20)
		}
		if p := g.onLow.Load(); p != nil && *p != nil {
			(*p)(low, free)
		}
	}
	return !low
}
