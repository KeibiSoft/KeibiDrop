// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: -race guard that Getattr keeps remFile.stat under metaMu after the
// ABOUTME: lstat was hoisted out of the lock (catches a future metaMu drop).

package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// TestGetattrStatMetaMu runs Getattr (which lstats then mutates remFile.stat in
// place) concurrently with OpenEx (which reads remFile.stat) on the same remote
// file that also exists on disk, so Getattr takes its size/mtime mutation path.
// Both must touch remFile.stat under metaMu; if the lstat hoist ever drops that
// guard, -race flags the stat read/write. The lstat is now outside metaMu; the
// stat access stays inside it.
//
// All files are seeded BEFORE the racing phase on purpose: a concurrent
// AddRemoteFile (a pending RemoteFilesLock writer) exposes a SEPARATE,
// pre-existing Getattr/OpenEx lock-order deadlock (Getattr holds
// RemoteFilesLock.RLock across Adm+AfmLock while OpenEx holds AfmLock and wants
// RemoteFilesLock.RLock). That is out of scope for this metaMu guard.
func TestGetattrStatMetaMu(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.SetStreamProvider(func() types.FileStreamProvider { return nil })
	lg := nopLogger()

	const numFiles = 12
	const iters = 200

	paths := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/g_%04d", i)
		name := fmt.Sprintf("g_%04d", i)
		paths[i] = path
		// Remote file with an OLD mtime so the on-disk copy looks newer and
		// Getattr takes the stat-mutation branch.
		rstat := &winfuse.Stat_t{Size: 64}
		rstat.Mtim.Sec = 1
		_ = d.AddRemoteFile(lg, path, name, rstat)
		// On-disk copy at cleanPath (LocalDownloadFolder/name) so platLstat hits.
		if err := os.WriteFile(filepath.Join(saveDir, name), make([]byte, 64), 0644); err != nil {
			t.Fatalf("write local %s: %v", name, err)
		}
	}

	var wg sync.WaitGroup
	for _, path := range paths {
		wg.Add(2)
		go func(p string) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				var gs winfuse.Stat_t
				d.Getattr(p, &gs, 0)
			}
		}(path)
		go func(p string) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				fi := &winfuse.FileInfo_t{Flags: syscall.O_RDONLY}
				d.OpenEx(p, fi)
				if fi.Fh != 0 {
					d.Release(p, fi.Fh)
				}
			}
		}(path)
	}
	wg.Wait()
}
