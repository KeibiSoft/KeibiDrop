// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package synctracker

import (
	"os"
	"sync"
	"sync/atomic"
)

type File struct {
	Name string

	RelativePath   string // Path relative to the root of the mounted filesystem.
	RealPathOfFile string // Path on the local filesystem.

	LastEditTime uint64 // Use time.Now().UnixNano().
	CreatedTime  uint64
	Atime        uint64 // Announced access time, unix nanoseconds. 0 means unknown.

	Size uint64
	Mode uint32 // Announced mode bits. 0 means unknown; presenters fall back to 0644.
}

// listWrites counts every write to the file lists of any tracker, and every
// new tracker (a session swap replaces it). A poller that reads the same
// number twice knows nothing changed in between, without taking a lock.
var listWrites atomic.Uint64

// ListWrites returns that count.
func ListWrites() uint64 { return listWrites.Load() }

// RWMutex is a sync.RWMutex whose write unlock bumps listWrites. The count
// adds no waiting: Unlock bumps it and returns.
type RWMutex struct {
	sync.RWMutex
}

// Unlock counts the write first, so a reader that sees the new count and
// then takes the lock reads the new map.
func (m *RWMutex) Unlock() {
	listWrites.Add(1)
	m.RWMutex.Unlock()
}

type SyncTracker struct {
	LocalFilesMu RWMutex
	LocalFiles   map[string]*File

	RemoteFilesMu RWMutex
	RemoteFiles   map[string]*File
}

func NewSyncTracker() *SyncTracker {
	listWrites.Add(1)
	return &SyncTracker{
		LocalFiles:  make(map[string]*File),
		RemoteFiles: make(map[string]*File),
	}
}

// PruneStaleLocalFiles removes entries whose underlying file no longer exists.
// The disk checks run outside the lock: one stat per shared file under the
// write lock stalled every reader of the list. The write lock is taken only
// when something is gone, and an entry replaced meanwhile stays.
func (st *SyncTracker) PruneStaleLocalFiles() {
	type entry struct {
		name string
		file *File
		path string // copied under the lock: the check runs without it
	}
	st.LocalFilesMu.RLock()
	shared := make([]entry, 0, len(st.LocalFiles))
	for name, f := range st.LocalFiles {
		if f.RealPathOfFile != "" {
			shared = append(shared, entry{name, f, f.RealPathOfFile})
		}
	}
	st.LocalFilesMu.RUnlock()

	var gone []entry
	for _, e := range shared {
		if _, err := os.Stat(e.path); err != nil {
			gone = append(gone, e)
		}
	}
	if len(gone) == 0 {
		return
	}
	st.LocalFilesMu.Lock()
	for _, e := range gone {
		if st.LocalFiles[e.name] == e.file {
			delete(st.LocalFiles, e.name)
		}
	}
	st.LocalFilesMu.Unlock()
}
