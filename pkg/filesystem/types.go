// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
)

// DownloadState tracks download progress for resumption on reconnect.
type DownloadState struct {
	TotalSize       atomic.Uint64 // Expected total bytes from peer.
	BytesDownloaded atomic.Uint64 // Bytes successfully written to local cache.
	LastReadOffset  atomic.Int64  // Last successfully read offset.
	StartedAt       atomic.Int64  // Unix nano when download started.
	LastSuccessAt   atomic.Int64  // Unix nano of last successful read.
	AttemptCount    atomic.Int32  // Reconnection attempts since last success.
	MaxRetries      int           // Max retries before giving up (default 5).

	// Checksum tracking using xxHash3 (~30GB/s, non-cryptographic).
	// Used to verify download integrity, not for security (data is already authenticated).
	hasher   *xxh3.Hasher
	hasherMu sync.Mutex
}

// Reset clears download state for a new download.
func (ds *DownloadState) Reset(totalSize uint64) {
	ds.TotalSize.Store(totalSize)
	ds.BytesDownloaded.Store(0)
	ds.LastReadOffset.Store(0)
	ds.StartedAt.Store(time.Now().UnixNano())
	ds.LastSuccessAt.Store(time.Now().UnixNano())
	ds.AttemptCount.Store(0)

	// Initialize fresh hasher.
	ds.hasherMu.Lock()
	ds.hasher = xxh3.New()
	ds.hasherMu.Unlock()
}

// UpdateProgress records successful read progress and updates checksum.
func (ds *DownloadState) UpdateProgress(offset int64, bytesRead int) {
	ds.BytesDownloaded.Add(uint64(bytesRead))
	newOffset := offset + int64(bytesRead)
	// Only update LastReadOffset if this extends our progress.
	for {
		current := ds.LastReadOffset.Load()
		if newOffset <= current {
			break
		}
		if ds.LastReadOffset.CompareAndSwap(current, newOffset) {
			break
		}
	}
	ds.LastSuccessAt.Store(time.Now().UnixNano())
	ds.AttemptCount.Store(0) // Reset retry count on success.
}

// UpdateChecksum adds data to the running checksum.
// Call this with the actual bytes received (in order received, not by offset).
func (ds *DownloadState) UpdateChecksum(data []byte) {
	ds.hasherMu.Lock()
	if ds.hasher != nil {
		_, _ = ds.hasher.Write(data)
	}
	ds.hasherMu.Unlock()
}

// Checksum returns the current xxHash3 checksum of received data.
func (ds *DownloadState) Checksum() uint64 {
	ds.hasherMu.Lock()
	defer ds.hasherMu.Unlock()
	if ds.hasher == nil {
		return 0
	}
	return ds.hasher.Sum64()
}

// CanRetry checks if we should attempt reconnection.
func (ds *DownloadState) CanRetry() bool {
	maxRetries := ds.MaxRetries
	if maxRetries == 0 {
		maxRetries = 5 // Default.
	}
	return int(ds.AttemptCount.Load()) < maxRetries
}

// RecordAttempt increments the retry counter.
func (ds *DownloadState) RecordAttempt() int32 {
	return ds.AttemptCount.Add(1)
}

// Progress returns download completion percentage (0-100).
func (ds *DownloadState) Progress() float64 {
	total := ds.TotalSize.Load()
	if total == 0 {
		return 0
	}
	return float64(ds.BytesDownloaded.Load()) / float64(total) * 100
}

// IsComplete returns true if all bytes have been downloaded.
func (ds *DownloadState) IsComplete() bool {
	total := ds.TotalSize.Load()
	return total > 0 && ds.BytesDownloaded.Load() >= total
}

// The plan is like this:
// Mounted filesystem for Alice is visible at "mountedPath".
// When Alice adds files from her machine to the filesystem,
// *Blip blip blop*, a new File{} gets created which represents
// a "symlink" to the original file which Bob can now access.
// For folders it's a bit different, as it does not create a "symlink"
// in the sense that Bob can navigate outside of the mapped folder
// but in the sense that Dir{} is mapped to the underlying one
// and all the children of the underlying one are mapped inside our Dir{}.

// Note: I use a tree hierarchy, not the most efficient way when it comes to lookups.
// I might flatten it in the future.
// Root -> Dir -> Dir -> File
//    | -> File
//    | -> Dir -> File

type Dir struct {
	logger *slog.Logger

	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Relative (to root) path in the mounted filesystem.
	RealPathOfFile string `json:"pathOnLocalSystem"` // The Path on the local system.

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	LocalDownloadFolder string // The folder where the files from the peer are downloaded.

	Parent *Dir
	Root   *Dir

	OpenFileHandlers map[uint64]*File
	OpenMapLock      sync.RWMutex

	Adm       sync.RWMutex
	AllDirMap map[string]*Dir

	AfmLock    sync.RWMutex
	AllFileMap map[string]*File

	stat *winfuse.Stat_t

	OnLocalChange      func(event types.FileEvent)
	OpenStreamProvider func() types.FileStreamProvider

	// Collab sync options (propagated from FS).
	PrefetchOnOpen bool // If true, fetch entire file on Open() and write to local disk.
	PushOnWrite    bool // If true, async push deltas to peer on Write().

	RemoteFilesLock sync.RWMutex
	RemoteFiles     map[string]*File
}

type File struct {
	logger *slog.Logger

	Inode uint64 `json:"inode"` // Inodes must be unique and not re-used.
	Name  string `json:"name"`

	RelativePath string `json:"relativePath"` // Relative (to root) path in the mounted filesystem.

	RealPathOfFile string // The Path on the local system.

	Parent *Dir
	Root   *Dir

	LastEditTime uint64 `json:"lastEdit"` // Use time.Now().UnixNano().
	CreatedTime  uint64 `json:"createdAt"`

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	NotLocalSynced  bool
	NotRemoteSynced bool

	LocalNewer bool

	HadEdits bool

	// WasTruncatedToZero tracks if Truncate(size=0) was explicitly called.
	// Used with HadEdits to distinguish legitimate empty files from transient states.
	WasTruncatedToZero bool

	// LastNotifiedSize tracks the file size we last sent to peer in ADD_FILE.
	// Used to avoid sending duplicate notifications with same size during file copy.
	LastNotifiedSize int64

	// PeerStoppedSharing is set when peer sends REMOVE_FILE but download is in progress.
	// Once download completes (Release with 0 open handles), the file reference is removed.
	PeerStoppedSharing bool

	openFileCounter OpenFileCounter

	StreamProvider   types.FileStreamProvider
	RemoteFileStream types.RemoteFileStream
	StreamCancel     context.CancelFunc // Cancel function for the stream context

	// Download resumption state.
	Download DownloadState

	// Bitmap tracks which 512 KiB chunks have been downloaded from the remote peer.
	// nil for local-origin files or empty files (size=0).
	Bitmap *ChunkBitmap

	// PrefetchCancel cancels the background prefetch goroutine for this file.
	PrefetchCancel context.CancelFunc

	OnLocalChange func(event types.FileEvent)

	stat *winfuse.Stat_t
}

func (f *File) NotifyPeer() {}

// CountOpenDescriptors returns the number of open file handles.
func (f *File) CountOpenDescriptors() uint64 {
	return f.openFileCounter.CountOpenDescriptors()
}

// Use it as a singleton only when setting up the filesystem.
// (In the mount command).
// I do not enforce it as a singleton, as my philospohy
// is to not have package global var, just a
// call chain of functions from the entrypoint of
// the program.

// Create and Open calls must have a corresponding Release call.
type OpenFileCounter struct {
	mu      *sync.Mutex
	counter uint64
}

func (ofc *OpenFileCounter) Open() {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	ofc.counter++
}

func (ofc *OpenFileCounter) Release() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	if ofc.counter == 0 {
		return 0
	}

	ofc.counter--
	return ofc.counter
}

func (ofc *OpenFileCounter) CountOpenDescriptors() uint64 {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	return ofc.counter
}
