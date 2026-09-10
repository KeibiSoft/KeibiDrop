// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: The .kdbitmap sidecar of an on-demand cache copy: written as blocks land,
// ABOUTME: kept when the copy completes, adopted at the next session before the announce.

package filesystem

import (
	"os"
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
	f.metaMu.RUnlock()
	if bm == nil || real == "" {
		return
	}
	if err := bm.Save(BitmapPath(real)); err != nil && f.logger != nil {
		f.logger.Debug("Sidecar write failed", "path", real, "error", err)
	}
}

// adoptCacheCopy turns a save-folder file that carries a sidecar into the cache
// copy it is: bitmap-gated and remote-backed, never a plain local file. Runs on an
// open before the peer's announce (a stat or open in the first seconds of a
// session, after a daemon restart). Seen 2026-09-09 (BUGS 29): in those seconds a
// partial copy served zeros at disk speed, and the zero pages then sat in the
// kernel's page cache for the rest of the session. A file without a sidecar is
// left alone: it is a local file, or a copy from a build that wrote none.
func (d *Dir) adoptCacheCopy(f *File, localPath string) {
	stgo, err := platLstat(localPath)
	if err != nil || stgo.Size <= 0 {
		return
	}
	bm, err := LoadChunkBitmap(BitmapPath(localPath), stgo.Size)
	if err != nil {
		return
	}
	f.Bitmap = bm
	f.NotLocalSynced = !bm.IsComplete()
	f.LocalNewer = false
	f.IsLocalPresent = true
	if f.stat == nil {
		f.stat = new(winfuseStat)
	}
	*f.stat = stgo
	if d.logger != nil {
		d.logger.Debug("Adopted a cache copy from its sidecar", "path", f.RelativePath, "have", bm.Have(), "total", bm.Total())
	}
}

// dropSidecar removes the sidecar of a path that is being rewritten or removed.
func dropSidecar(localPath string) {
	_ = os.Remove(BitmapPath(localPath))
}
