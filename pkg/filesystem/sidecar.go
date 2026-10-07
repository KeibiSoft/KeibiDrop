// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: The .kdbitmap sidecar of an on-demand cache copy: written as blocks land,
// ABOUTME: kept when the copy completes, adopted at the next session before the announce.

//go:build !android

package filesystem

import (
	"os"
	"path/filepath"
	"sync"
	"time"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

type winfuseStat = winfuse.Stat_t

// sidecarSaveDelay coalesces sidecar writes. A sequential read lands a unit every
// few milliseconds and the sidecar is the whole bitmap each time, so one write a
// second is plenty; Release flushes the rest.
const sidecarSaveDelay = time.Second

// noteLanded schedules a sidecar write after bytes landed in the cache copy. The
// sidecar is what makes the copy identifiable at the next session (BUGS 29):
// without it a partial copy is a plain local file that serves zeros until the
// peer's announce arrives, and a complete one is fetched again because nothing
// says it is complete. Prefetch has always written one; on-demand reads did not.
func (f *File) noteLanded() {
	f.sidecarMu.Lock()
	defer f.sidecarMu.Unlock()
	if f.sidecarTimer == nil {
		f.sidecarTimer = time.AfterFunc(sidecarSaveDelay, f.flushSidecar)
	}
}

// flushSidecar writes the sidecar now, if a bitmap exists, and clears any pending
// timer. Safe to call from Release and from the timer.
func (f *File) flushSidecar() {
	f.sidecarMu.Lock()
	if f.sidecarTimer != nil {
		f.sidecarTimer.Stop()
		f.sidecarTimer = nil
	}
	f.sidecarMu.Unlock()
	f.metaMu.RLock()
	bm := f.Bitmap
	real := f.RealPathOfFile
	meta := f.sidecarMetaLocked()
	f.metaMu.RUnlock()
	if bm == nil || real == "" {
		return
	}
	if meta.Ledger != nil {
		meta.Ledger.syncFromBitmap(bm, meta.HeldStamp)
	}
	if err := SaveSidecar(BitmapPath(real), bm, meta); err != nil && f.logger != nil {
		f.logger.Debug("Sidecar write failed", "path", real, "error", err)
	}
}

// flushPendingSidecars writes now every sidecar still waiting on its timer.
// Unmount calls it: a timer that fires later writes into a folder the caller
// may be removing, and one still pending when the process exits is lost. A
// warmed sibling has no handle, so no Release flushes its sidecar.
func (d *Dir) flushPendingSidecars() {
	seen := make(map[*File]struct{})
	var files []*File
	collect := func(f *File) {
		if _, ok := seen[f]; f != nil && !ok {
			seen[f] = struct{}{}
			files = append(files, f)
		}
	}
	d.AfmLock.RLock()
	for _, f := range d.AllFileMap {
		collect(f)
	}
	d.AfmLock.RUnlock()
	d.RemoteFilesLock.RLock()
	for _, f := range d.RemoteFiles {
		collect(f)
	}
	d.RemoteFilesLock.RUnlock()
	for _, f := range files {
		f.sidecarMu.Lock()
		pending := f.sidecarTimer != nil
		f.sidecarMu.Unlock()
		if pending {
			f.flushSidecar()
		}
	}
}

// sidecarMetaLocked composes the v2 metadata; metaMu held (read). The edit
// base travels only with a dirty record: it is the announce's base.
func (f *File) sidecarMetaLocked() *SidecarMeta {
	m := &SidecarMeta{HeldStamp: f.HeldMtimeNs, Ledger: f.Ledger}
	if f.Ledger != nil && f.Ledger.AnyDirty() {
		m.EditBase = f.EditBaseMtimeNs
	}
	return m
}

// flushSidecarSynced makes the bytes durable before the sidecar names them
// (CHUNK-LEDGER-DESIGN.md 3.3): a crash between the two leaves a chunk
// unmarked and refetched, never a record without bytes. One fsync per save,
// off the FUSE path: Release and the fill run it in a goroutine.
func (f *File) flushSidecarSynced() {
	f.metaMu.RLock()
	real := f.RealPathOfFile
	f.metaMu.RUnlock()
	if real != "" {
		if fd, err := OpenShared(real, os.O_WRONLY, 0); err == nil {
			_ = fd.Sync()
			_ = fd.Close()
		}
	}
	f.flushSidecar()
}

// adoptCacheCopy turns a save-folder file that carries a sidecar into the cache
// copy it is: bitmap-gated and remote-backed, never a plain local file. Runs on an
// open before the peer's announce (a stat or open in the first seconds of a
// session, after a daemon restart). Seen 2026-09-09 (BUGS 29): in those seconds a
// partial copy served zeros at disk speed, and the zero pages then sat in the
// kernel's page cache for the rest of the session. A file without a sidecar is
// left alone: it is a local file, or a copy from a build that wrote none.
func (d *Dir) adoptCacheCopy(f *File, localPath string) {
	d.adoptSidecarInto(f, localPath)
}

// adoptSidecarInto loads the sidecar of the copy at localPath into f: the
// bitmap, and from a v2 sidecar the version the bytes came from and the
// unannounced edit (restoreSidecarMetaLocked). The size on disk is the
// copy's size: a peer version of another size is then an edit of it, judged
// by the announce path with the restored state. False without a sidecar.
func (d *Dir) adoptSidecarInto(f *File, localPath string) bool {
	stgo, err := platLstat(localPath)
	if err != nil || stgo.Size <= 0 {
		return false
	}
	bm, meta, err := LoadSidecar(BitmapPath(localPath), stgo.Size)
	if err != nil {
		return false
	}
	f.metaMu.Lock()
	f.Bitmap = bm
	f.NotLocalSynced = !bm.IsComplete()
	f.LocalNewer = false
	f.IsLocalPresent = true
	if f.stat == nil {
		f.stat = new(winfuseStat)
	}
	*f.stat = stgo
	restored := f.restoreSidecarMetaLocked(meta)
	f.metaMu.Unlock()
	if d.logger != nil {
		d.logger.Debug("Adopted a cache copy from its sidecar", "path", f.RelativePath,
			"have", bm.Have(), "total", bm.Total(), "unannouncedEdit", restored)
	}
	return true
}

// restoreSidecarMetaLocked applies a v2 sidecar's metadata to f, metaMu
// held: the held version these bytes belong to, so the peer's re-announce of
// it is a redelivery and a later swap carries the right base; and the dirty
// records of an edit the fill never finished announcing, which make the file
// local-newer again with its base, so the announce path resumes the fill and
// the conflict rule still sees the edit (CHUNK-LEDGER-DESIGN.md 3.3). The
// identity is raised to the newest dirty stamp: the announce must not carry
// a stamp below what the records minted. Returns true when an edit was
// restored.
func (f *File) restoreSidecarMetaLocked(meta *SidecarMeta) bool {
	if meta == nil {
		return false
	}
	if meta.HeldStamp > f.HeldMtimeNs {
		f.HeldMtimeNs = meta.HeldStamp
	}
	if meta.HeldStamp > f.RemoteMtimeNs {
		f.RemoteMtimeNs = meta.HeldStamp
	}
	f.Ledger = meta.Ledger
	if meta.Ledger == nil || !meta.Ledger.AnyDirty() {
		return false
	}
	f.LocalNewer = true
	f.NotRemoteSynced = true
	f.HadEdits = true
	f.AnnounceAfterFill = true
	f.EditBaseMtimeNs = meta.EditBase
	if f.EditBaseMtimeNs == 0 && f.RemoteMtimeNs > 0 {
		f.EditBaseMtimeNs = -1 // never held a version: the fresh-create class, as Write rules
	}
	if st := meta.Ledger.MaxDirtyStamp(); f.stat != nil && st > f.stat.Mtim.Sec*1e9+f.stat.Mtim.Nsec {
		f.stat.Mtim = winfuse.NewTimespec(time.Unix(0, st))
	}
	return true
}

// adoptFromDisk registers the cache copy at path when the peer announces it
// before any stat or open of this session touched it (a daemon restart): the
// copy is on disk with its sidecar, so the announce must be judged against
// what it holds, including an edit the fill never announced. Nil without a
// copy or a sidecar; the caller then takes the new-file path as before.
func (d *Dir) adoptFromDisk(path string) *File {
	localPath := filepath.Clean(filepath.Join(d.RealPathOfFile, path))
	f := &File{
		logger:          d.logger,
		openFileCounter: OpenFileCounter{mu: &sync.Mutex{}},
		Name:            getNameFromPath(path),
		RelativePath:    path,
		RealPathOfFile:  localPath,
		Root:            d,
		StreamProvider:  d.OpenStreamProvider(),
		stat:            &winfuse.Stat_t{},
	}
	if !d.adoptSidecarInto(f, localPath) {
		return nil
	}
	return f
}

// dropSidecar removes the sidecar of a path that is being rewritten or removed.
func dropSidecar(localPath string) {
	_ = os.Remove(BitmapPath(localPath))
}

// adoptForAnnounce gives an announce the cache copy a daemon restart left on
// disk: an entry a stat registered as a plain file gets its sidecar back, and
// a path with no entry at all gets one from the copy. Both then carry the
// version the bytes came from and any edit the fill never announced, so the
// announce is judged against them (restoreSidecarMetaLocked). A path the
// peer already tracks, or a copy without a sidecar, is left to the paths as
// before.
func (d *Dir) adoptForAnnounce(path string, local *File, hasLocal bool) (*File, bool) {
	if hasLocal {
		local.metaMu.RLock()
		plain := local.Bitmap == nil && !local.LocalNewer && local.RealPathOfFile != ""
		real := local.RealPathOfFile
		local.metaMu.RUnlock()
		if plain {
			d.adoptSidecarInto(local, real)
		}
		return local, true
	}
	d.RemoteFilesLock.RLock()
	_, known := d.RemoteFiles[path]
	d.RemoteFilesLock.RUnlock()
	if known {
		return nil, false
	}
	f := d.adoptFromDisk(path)
	if f == nil {
		return nil, false
	}
	d.AfmLock.Lock()
	if cur, ok := d.AllFileMap[path]; ok {
		f = cur
	} else {
		d.AllFileMap[path] = f
	}
	d.AfmLock.Unlock()
	return f, true
}
