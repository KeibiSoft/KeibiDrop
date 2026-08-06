//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
)

// HandleEntry maps an opaque FUSE handle ID to the kernel fd and File metadata.
// Handle IDs only increase and never repeat, so a kernel fd number reused after
// close() causes no race.
type HandleEntry struct {
	FD   int // Kernel file descriptor for syscalls.
	File *File
}

var nextHandleID atomic.Uint64

func allocHandleID() uint64 {
	return nextHandleID.Add(1)
}

// DownloadState tracks download progress for resume after reconnect.
type DownloadState struct {
	TotalSize       atomic.Uint64 // Expected total bytes from peer.
	BytesDownloaded atomic.Uint64 // Bytes successfully written to local cache.
	LastReadOffset  atomic.Int64  // Last successfully read offset.
	StartedAt       atomic.Int64  // Unix nano when download started.
	LastSuccessAt   atomic.Int64  // Unix nano of last successful read.
	AttemptCount    atomic.Int32  // Reconnection attempts since last success.
	MaxRetries      int           // Maximum retries before the download stops (default 5).

	// Checksum state uses xxHash3 (~30GB/s, non-cryptographic).
	// It verifies download integrity, not security. The data is already authenticated.
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

	ds.hasherMu.Lock()
	ds.hasher = xxh3.New()
	ds.hasherMu.Unlock()
}

// UpdateProgress records successful read progress and updates the checksum.
func (ds *DownloadState) UpdateProgress(offset int64, bytesRead int) {
	ds.BytesDownloaded.Add(uint64(bytesRead))
	newOffset := offset + int64(bytesRead)
	// Advance LastReadOffset only if the new offset is larger.
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
// Pass bytes in receive order, not offset order.
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

// CanRetry reports whether the download can try another reconnect.
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

// IsComplete reports whether all bytes are downloaded.
func (ds *DownloadState) IsComplete() bool {
	total := ds.TotalSize.Load()
	return total > 0 && ds.BytesDownloaded.Load() >= total
}

// Mapping model: the mounted filesystem is visible at "mountedPath".
// A shared file maps to a File{} that points to the source path, like a symlink.
// A shared folder maps to a Dir{}. Children of the source directory map to
// entries inside the Dir{}. The peer cannot navigate outside the mapped folder.

// The hierarchy is a tree, not a flat map. Tree lookups cost more.
// Root -> Dir -> Dir -> File
//    | -> File
//    | -> Dir -> File

type Dir struct {
	logger *slog.Logger

	Inode uint64 `json:"inode"` // Inodes must be unique and never reused.
	Name  string `json:"name"`

	RelativePath   string `json:"relativePath"`      // Path relative to the mount root.
	RealPathOfFile string `json:"pathOnLocalSystem"` // Path on the local system.

	PeerLastEdit   uint64 `json:"peerLastEdit"`
	IsLocalPresent bool   `json:"isLocalPresent"`

	LocalDownloadFolder string // Folder that stores files downloaded from the peer.

	Parent *Dir
	Root   *Dir

	OpenFileHandlers map[uint64]*HandleEntry
	OpenMapLock      sync.RWMutex

	Adm       sync.RWMutex
	AllDirMap map[string]*Dir

	AfmLock    sync.RWMutex
	AllFileMap map[string]*File

	stat *winfuse.Stat_t

	// onLocalChange and openStreamProvider live on the ROOT dir. Reconnect
	// re-publishes them via SetCallbacks while FUSE threads read them, so
	// access is atomic. Child dirs route through Root; the OnLocalChange and
	// OpenStreamProvider methods wrap the loads.
	onLocalChange      atomic.Pointer[func(event types.FileEvent)]
	openStreamProvider atomic.Pointer[func() types.FileStreamProvider]

	// Collab sync options (propagated from FS).
	PrefetchOnOpen bool // If true, Open() fetches the whole file and writes it to local disk.
	PrefetchAutoMB int  // Files at or above this many MB prefetch on open (0=off).
	PushOnWrite    bool // If true, Write() pushes deltas to the peer asynchronously.

	// ReadAheadWindowBlocks caps predictive sequential read-ahead. Sequential
	// reads fetch up to this many ReadAheadBlock-sized blocks ahead of the read
	// head, so a high-RTT link does not stall at each block boundary. The in-use
	// window self-tunes by hit/miss feedback and never exceeds this cap. A slow
	// consumer (e.g. video) settles well below it. 0 = off (pure on-demand).
	// The value comes from config read_ahead_window_mb.
	ReadAheadWindowBlocks int

	// raPrefetchCalls counts read-ahead prefetch calls for observability: one per
	// block the read head crosses, not one per read. Tests assert the count stays
	// proportional to blocks, not to the number of small reads.
	raPrefetchCalls atomic.Int64

	RemoteFilesLock sync.RWMutex
	RemoteFiles     map[string]*File

	// PrefetchSem limits concurrent prefetch goroutines. Without a limit, a large
	// clone (600+ files) opens 600+ parallel StreamFile gRPC streams and
	// overwhelms the connection.
	PrefetchSem chan struct{}

	// fsCtx is the live FUSE context, on the ROOT dir. Disconnect swaps it via
	// SetCtx while FUSE threads read it, so access is atomic. FS.Unmount()
	// cancels it to unblock FUSE handlers stuck on gRPC.
	fsCtx atomic.Pointer[context.Context]
}

// Ctx returns the live FUSE context from the root. Disconnect swaps it, so do
// not cache the returned context across sessions.
func (d *Dir) Ctx() context.Context {
	r := d.Root
	if r == nil {
		r = d
	}
	if p := r.fsCtx.Load(); p != nil {
		return *p
	}
	return context.Background()
}

// SetCtx publishes a fresh FUSE context. Call it on the root.
func (d *Dir) SetCtx(ctx context.Context) {
	d.fsCtx.Store(&ctx)
}

// OnLocalChange reports a local file event to the session. It routes through
// the root's atomically published callback and does nothing while no session
// callback is wired.
func (d *Dir) OnLocalChange(event types.FileEvent) {
	r := d.Root
	if r == nil {
		r = d
	}
	if p := r.onLocalChange.Load(); p != nil && *p != nil {
		(*p)(event)
	}
}

// SetOnLocalChange publishes the local-change callback. Call it on the root.
func (d *Dir) SetOnLocalChange(fn func(event types.FileEvent)) {
	d.onLocalChange.Store(&fn)
}

// hasOnLocalChange reports whether a local-change callback is currently
// published on the root.
func (d *Dir) hasOnLocalChange() bool {
	r := d.Root
	if r == nil {
		r = d
	}
	p := r.onLocalChange.Load()
	return p != nil && *p != nil
}

// OpenStreamProvider returns the current session's stream provider, or nil
// while no session factory is wired. It routes through the root's atomically
// published factory.
func (d *Dir) OpenStreamProvider() types.FileStreamProvider {
	r := d.Root
	if r == nil {
		r = d
	}
	if p := r.openStreamProvider.Load(); p != nil && *p != nil {
		return (*p)()
	}
	return nil
}

// StreamProviderFn returns the published factory itself, for callers that
// wrap and restore it (tests).
func (d *Dir) StreamProviderFn() func() types.FileStreamProvider {
	r := d.Root
	if r == nil {
		r = d
	}
	if p := r.openStreamProvider.Load(); p != nil {
		return *p
	}
	return nil
}

// SetStreamProvider publishes the stream-provider factory. Call it on the root.
func (d *Dir) SetStreamProvider(fn func() types.FileStreamProvider) {
	d.openStreamProvider.Store(&fn)
}

// SetCallbacks publishes both session callbacks. Call it on the root.
func (d *Dir) SetCallbacks(onLocalChange func(event types.FileEvent), provider func() types.FileStreamProvider) {
	d.SetOnLocalChange(onLocalChange)
	d.SetStreamProvider(provider)
}

type File struct {
	logger *slog.Logger

	Inode           uint64 `json:"inode"` // Inodes must be unique and never reused.
	CurrentHandleID uint64 // Opaque FUSE handle ID for the currently-open fd.
	Name            string `json:"name"`

	RelativePath string `json:"relativePath"` // Path relative to the mount root.

	RealPathOfFile string // Path on the local system.

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

	// RemoteMtimeNs is the newest peer-announced mtime (UnixNano). Only
	// notifications set it, under RemoteFilesLock. Local ops overwrite
	// stat.Mtim and must not join the staleness comparison.
	RemoteMtimeNs int64

	// EditBaseMtimeNs is max(HeldMtimeNs, LastAnnouncedMtimeNs), snapshotted
	// at the first dirtying op of an edit session under metaMu. The announce
	// carries it so the receiver can prove a concurrent edit: a base below the
	// version the receiver holds means the edit never saw that version.
	EditBaseMtimeNs int64

	// HeldMtimeNs is the newest version stamp whose BYTES are materialized
	// locally: own announces, landed fetches, and the startup scan raise it;
	// a metadata-only accept does not. The session base uses it, so an
	// announce can never claim a version this peer never held. Model v3
	// proves the rule: a blind overwrite then loses the base comparison and
	// the receiver preserves the only copy of the victim bytes.
	HeldMtimeNs int64

	// LastAnnouncedMtimeNs is the mtime of our newest announce for this file,
	// guarded by metaMu. RemoteMtimeNs records only the peer's announcements,
	// so without this an owner-side base degrades to unknown and a conflict on
	// its own file cannot be proved.
	LastAnnouncedMtimeNs int64

	// WasTruncatedToZero records an explicit Truncate(size=0) call. With
	// HadEdits, it separates legitimate empty files from transient states.
	WasTruncatedToZero bool

	// LastNotifiedSize is the file size last sent to the peer in ADD_FILE. It
	// prevents duplicate same-size notifications during a file copy.
	LastNotifiedSize int64

	// PeerStoppedSharing is set when the peer sends REMOVE_FILE during a download.
	// On Release with 0 open handles, the code removes the file reference.
	PeerStoppedSharing bool

	openFileCounter OpenFileCounter

	StreamProvider types.FileStreamProvider
	StreamPool     *StreamPool        // Pool of parallel gRPC streams for on-demand reads.
	StreamCancel   context.CancelFunc // Cancel function for the stream context.
	CacheFD        *os.File           // Persistent cache file descriptor for on-demand writes.
	CacheWg        sync.WaitGroup     // Tracks in-flight async cache writes. Release waits for them.

	// fetchMu guards inflight: in-flight read-ahead block fetches keyed by block
	// start offset. Concurrent reads of the same block coalesce onto the leader's
	// single fetch instead of each pulling the whole block over the network.
	fetchMu  sync.Mutex
	inflight map[int64]*blockFetch

	// Predictive read-ahead detector state, guarded by raMu. raMu is a leaf lock:
	// hold it only for these field updates, never across I/O or another lock.
	//
	// CRITICAL: FUSE delivers the reads of one sequential scan in parallel across
	// worker threads, so they arrive here out of order. A stride test against the
	// previous read sees constant backward jumps and classifies every read as a
	// seek, so read-ahead never fires. The detector instead tracks the read head
	// as a high-water mark (raHeadOffset = max end offset seen). It classifies
	// each read by distance from that mark: within +/- a window the read belongs
	// to the current stream (parallel laggards included); far away it is a real
	// seek. The test does not depend on arrival order.
	//
	// raStreamActive: a stream is established (false on a fresh file or right
	// after a seek). raStreamBytes: bytes read since the stream began (reset on
	// seek). The thumbnail gate fires read-ahead only after real consumption
	// passes a threshold, so a scattered probe, which advances the head but reads
	// almost nothing, never prefetches. raHeadOffset: the stream's leading edge
	// (high-water mark). raWindowBlocks: blocks to keep ahead (1 after a seek,
	// primed to the cap on a confirmed stream). raPrefetchedTo: the prefetch
	// frontier (next block index not yet prefetched). Read-ahead fires at most
	// once per block the head crosses, not on every small read.
	raMu           sync.Mutex
	raStreamActive bool
	raStreamBytes  int64
	raHeadOffset   int64
	raWindowBlocks int
	raPrefetchedTo int64
	raPrefetching  bool               // A sequential prefetch goroutine is in flight (one per file).
	raCancel       context.CancelFunc // Cancels the in-flight prefetch range. Called on a seek.

	// Download resumption state.
	Download DownloadState

	// Bitmap tracks which 512 KiB chunks are downloaded from the remote peer.
	// It is nil for local-origin files and empty files (size=0).
	Bitmap *ChunkBitmap

	// PrefetchCancel cancels the background prefetch goroutine for this file.
	PrefetchCancel context.CancelFunc

	stat *winfuse.Stat_t

	// metaMu guards the File's mutable metadata. Concurrent FUSE ops reach that
	// metadata through different map locks (AllFileMap/AfmLock vs
	// OpenFileHandlers/OpenMapLock), so those locks do not mutually exclude.
	// Guarded: the *stat struct (Getattr mutates it in place while Read/OpenEx
	// read Size/Mode) and the sync-state bools IsLocalPresent/LocalNewer/
	// NotLocalSynced/NotRemoteSynced/HadEdits. The race detector confirms this.
	// Lock order: metaMu is innermost. Take it after any map lock. Never take a
	// map lock while holding metaMu; this prevents deadlock.
	metaMu sync.RWMutex
}

func (f *File) NotifyPeer() {}

// CountOpenDescriptors returns the number of open file handles.
func (f *File) CountOpenDescriptors() uint64 {
	return f.openFileCounter.CountOpenDescriptors()
}

// Create a single OpenFileCounter at filesystem setup (the mount command).
// The package does not enforce this: it keeps no package globals; state
// passes down the call chain from the program entrypoint.

// OpenFileCounter counts open file handles.
// Each Create and Open call must have a matching Release call.
type OpenFileCounter struct {
	mu      *sync.Mutex
	counter uint64
}

func (ofc *OpenFileCounter) Open() {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	ofc.counter++
}

// OpenIfActive increments the count and returns true only if it was already >0.
// This prevents reuse of a handle whose last Release runs teardown.
func (ofc *OpenFileCounter) OpenIfActive() bool {
	ofc.mu.Lock()
	defer ofc.mu.Unlock()
	if ofc.counter == 0 {
		return false
	}
	ofc.counter++
	return true
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
