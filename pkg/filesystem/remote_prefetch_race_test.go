// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Mount-free -race guard for prefetch/Read/Getattr/AddRemoteFile racing
// ABOUTME: on one *File's NotLocalSynced and stat (prefetch writes, Read reads).

package filesystem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// TestRemotePrefetchStateRace drives, on the SAME *File (the one AddRemoteFile
// publishes into RemoteFiles/AllFileMap), the concurrent access classes that touch
// its sync-state (NotLocalSynced) and stat through different map locks, so the Go
// race detector flags every access that is not yet under metaMu. It mounts no FUSE
// and never calls Dir.Write, isolating these accesses.
//
// Two file pools, on purpose, so neither trips the SEPARATE (out-of-scope) Bitmap
// or PrefetchCancel pointer race (those have their own guards; the os.Truncate
// stat.Size race lives in getattr_open_deadlock_test.go):
//
//	Pool C (single-chunk, prefetch completes): the prefetch goroutine writes
//	  NotLocalSynced and reads stat.Mode while Read reads NotLocalSynced and
//	  Getattr mutates stat. AddRemoteFile is not run here, so the Bitmap pointer
//	  is never replaced.
//	Pool A (multi-chunk, prefetch never completes): AddRemoteFile holds size AND
//	  mtime CONSTANT, so it stays on the same-size/incomplete NotLocalSynced-write
//	  branch and never replaces the Bitmap pointer, racing Read's NotLocalSynced read.
func TestRemotePrefetchStateRace(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	provider := &slowStreamProvider{content: make([]byte, 4096)}
	d.SetStreamProvider(func() types.FileStreamProvider { return provider })
	d.PrefetchOnOpen = true
	d.SetCtx(context.Background())
	d.PrefetchSem = make(chan struct{}, 8)
	lg := nopLogger()

	const numFiles = 10
	const iters = 200
	// Multi-chunk so the single-shot slow stream sets only chunk 0; the bitmap
	// stays incomplete and AddRemoteFile's same-size branch keeps writing NotLocalSynced.
	const bigSize = int64(4*ChunkSize + 7)

	var wg sync.WaitGroup

	// ---- Pool C: prefetch (writes NotLocalSynced/reads stat) vs Read vs Getattr.
	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/c_%04d", i)
		name := fmt.Sprintf("c_%04d", i)

		// Remote file, single chunk (prefetch completes), OLD mtime so Getattr's
		// on-disk-newer stat-mutation branch fires.
		seed := &winfuse.Stat_t{Size: 4096, Mode: 0o644 | winfuse.S_IFREG}
		seed.Mtim.Sec = 1
		if err := d.AddRemoteFile(lg, path, name, seed); err != nil {
			t.Fatalf("AddRemoteFile seed C failed: %v", err)
		}
		// On-disk copy with a NEWER mtime so Getattr mutates stat in place.
		if err := os.WriteFile(filepath.Join(saveDir, name), make([]byte, 4096), 0644); err != nil {
			t.Fatalf("write local C %s: %v", name, err)
		}

		// (a) prefetchFile directly, sequentially, in ONE goroutine per File: writes
		// f.NotLocalSynced and reads f.stat.Mode, no PrefetchCancel.
		wg.Add(1)
		go func(p, n string) {
			defer wg.Done()
			d.RemoteFilesLock.RLock()
			f := d.RemoteFiles[p]
			d.RemoteFilesLock.RUnlock()
			if f == nil {
				return
			}
			realPath := filepath.Join(saveDir, n)
			for k := 0; k < iters; k++ {
				d.prefetchFile(context.Background(), lg, f, p, realPath)
			}
		}(path, name)

		// (b) Read storm on an open handle: reads f.NotLocalSynced.
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			fi := &winfuse.FileInfo_t{Flags: syscall.O_RDONLY}
			if d.OpenEx(p, fi) != 0 || fi.Fh == 0 {
				return
			}
			defer d.Release(p, fi.Fh)
			buf := make([]byte, 256)
			for k := 0; k < iters; k++ {
				d.Read(p, buf, 0, fi.Fh)
			}
		}(path)

		// (c) Getattr storm: mutates remFile.stat in place (on-disk copy newer).
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				var gs winfuse.Stat_t
				d.Getattr(p, &gs, 0)
			}
		}(path)
	}

	// ---- Pool A: AddRemoteFile same-size NotLocalSynced write vs Read's read.
	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/a_%04d", i)
		name := fmt.Sprintf("a_%04d", i)

		seed := &winfuse.Stat_t{Size: bigSize, Mode: 0o644 | winfuse.S_IFREG}
		seed.Mtim.Sec = 1
		if err := d.AddRemoteFile(lg, path, name, seed); err != nil {
			t.Fatalf("AddRemoteFile seed A failed: %v", err)
		}

		// (b) Read storm: reads f.NotLocalSynced on the open handle.
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			fi := &winfuse.FileInfo_t{Flags: syscall.O_RDONLY}
			if d.OpenEx(p, fi) != 0 || fi.Fh == 0 {
				return
			}
			defer d.Release(p, fi.Fh)
			buf := make([]byte, 256)
			for k := 0; k < iters; k++ {
				d.Read(p, buf, 0, fi.Fh)
			}
		}(path)

		// (d) AddRemoteFile storm: CONSTANT size and mtime (equal to the seed) so it
		// stays on the same-size/incomplete NotLocalSynced-write branch and never
		// replaces the Bitmap pointer. Races that write against Read's NotLocalSynced read.
		wg.Add(1)
		go func(p, n string) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				st := &winfuse.Stat_t{Size: bigSize, Mode: 0o644 | winfuse.S_IFREG}
				st.Mtim.Sec = 1
				_ = d.AddRemoteFile(lg, p, n, st)
			}
		}(path, name)
	}

	wg.Wait()
}
