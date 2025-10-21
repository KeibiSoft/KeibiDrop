package synctracker

import (
	"sync"
)

type File struct {
	Name string

	RelativePath   string // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string // The Path on the local system.

	LastEditTime uint64 // Use time.Now().UnixNano().
	CreatedTime  uint64

	Size uint64
}

type SyncTracker struct {
	LocalFilesMu sync.RWMutex
	LocalFiles   map[string]*File

	RemoteFilesMu sync.RWMutex
	RemoteFiles   map[string]*File
}
