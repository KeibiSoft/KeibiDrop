//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"io/fs"
	"os"
	"time"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

// captureOriginMeta records the origin's mode and times from an adopted
// announce stat. Caller must hold f.metaMu or own f exclusively (creation).
// Getattr later mutates f.stat from the local partial file, so these fields
// are the only stable source for preserve_metadata.
func captureOriginMeta(f *File, stat *winfuse.Stat_t) {
	if stat == nil {
		return
	}
	f.OriginMode = stat.Mode
	f.OriginAtimeNs = stat.Atim.Sec*1e9 + stat.Atim.Nsec
	f.OriginMtimeNs = stat.Mtim.Sec*1e9 + stat.Mtim.Nsec
}

// ApplyOriginMetadata sets the origin's permission bits and times on a fully
// saved file. mtimeNs 0 means no origin data: do nothing. atimeNs 0 falls
// back to mtime. Chmod/Chtimes act on the backing file directly, not through
// FUSE, so no notify loop starts. Birth time is not settable on most
// filesystems and stays view-only.
func ApplyOriginMetadata(path string, mode uint32, atimeNs, mtimeNs int64) error {
	if mtimeNs == 0 {
		return nil
	}
	if perm := fs.FileMode(mode) & fs.ModePerm; perm != 0 {
		if err := os.Chmod(path, perm); err != nil {
			return err
		}
	}
	if atimeNs == 0 {
		atimeNs = mtimeNs
	}
	return os.Chtimes(path, time.Unix(0, atimeNs), time.Unix(0, mtimeNs))
}

// beginDiskWriter registers an open write handle (on-demand cacheFD or
// prefetch fd) on the saved file.
func (f *File) beginDiskWriter() {
	f.metaMu.Lock()
	f.diskWriters++
	f.metaMu.Unlock()
}

// dropDiskWriter unregisters a write handle without the apply check, for
// teardown sites that hold OpenMapLock and must not touch the disk.
func (f *File) dropDiskWriter() {
	f.metaMu.Lock()
	f.diskWriters--
	f.metaMu.Unlock()
}

// endDiskWriter unregisters a write handle. When it was the last one and the
// download is complete, applies the origin metadata to realPath. The two
// download paths land bytes concurrently and either can finish last; only
// the last writer applies, so no later write or handle close (Windows stamps
// LastWriteTime then) can restamp the restored times. Callers close their
// write handle first and must not hold OpenMapLock.
func (f *File) endDiskWriter(d *Dir, realPath string) {
	f.metaMu.Lock()
	defer f.metaMu.Unlock()
	f.diskWriters--
	if f.diskWriters != 0 || !d.PreserveMeta() {
		return
	}
	if f.Bitmap == nil || !f.Bitmap.IsComplete() {
		return
	}
	if f.HadEdits || f.LocalNewer {
		// Local edits own the file's times now.
		return
	}
	applyOriginMetaLocked(f, realPath)
}

// applyOriginMetaLocked applies f's captured origin metadata to realPath and
// aligns f.stat's view with it, so disk, FUSE view, and announce agree.
// Caller must hold f.metaMu; the disk apply itself is cheap (two syscalls).
func applyOriginMetaLocked(f *File, realPath string) {
	if f.OriginMtimeNs == 0 {
		return
	}
	if err := ApplyOriginMetadata(realPath, f.OriginMode, f.OriginAtimeNs, f.OriginMtimeNs); err != nil {
		f.logger.Warn("preserve_metadata apply failed", "path", realPath, "error", err)
		return
	}
	if f.stat != nil {
		atime := f.OriginAtimeNs
		if atime == 0 {
			atime = f.OriginMtimeNs
		}
		f.stat.Atim = winfuse.NewTimespec(time.Unix(0, atime))
		f.stat.Mtim = winfuse.NewTimespec(time.Unix(0, f.OriginMtimeNs))
		if f.OriginMode != 0 {
			f.stat.Mode = f.OriginMode
		}
	}
}
