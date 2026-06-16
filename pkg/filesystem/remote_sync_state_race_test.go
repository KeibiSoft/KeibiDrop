// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Mount-free -race guard for the remote sync-state race: OpenEx reads
// ABOUTME: File.LocalNewer under metaMu while AddRemoteFile writes it un-metaMu.

package filesystem

import (
	"fmt"
	"sync"
	"syscall"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// TestRemoteSyncStateRace drives the OpenEx read of a File's sync-state bools
// (LocalNewer/NotLocalSynced) concurrently with the AddRemoteFile/EditRemoteFile
// writers, on the SAME *File (which the writers publish into AllFileMap). OpenEx
// reads under metaMu; AddRemoteFile writes LocalNewer under RemoteFilesLock (not
// metaMu) -> data race. It mounts no FUSE and never calls Dir.Write, so it cannot
// trip the Release-path race or the cgofuse teardown race; it isolates this one.
func TestRemoteSyncStateRace(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.OpenStreamProvider = func() types.FileStreamProvider { return nil }
	lg := nopLogger()

	const numFiles = 16
	const iters = 200
	var wg sync.WaitGroup

	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/r_%04d", i)
		name := fmt.Sprintf("r_%04d", i)
		// Seed: creates the *File in RemoteFiles (and, on re-announce, AllFileMap).
		seed := &winfuse.Stat_t{Size: 1024}
		_ = d.AddRemoteFile(lg, path, name, seed)

		wg.Add(2)
		// Reader: OpenEx reads fh.LocalNewer (metaMu) at the top of the open path.
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
		// Writer: AddRemoteFile/EditRemoteFile write LocalNewer/NotLocalSynced and
		// re-publish the same *File into AllFileMap.
		go func(p, n string) {
			defer wg.Done()
			for k := 0; k < iters; k++ {
				st := &winfuse.Stat_t{Size: int64(1024 + k%5)}
				st.Mtim.Sec = int64(k + 1)
				_ = d.AddRemoteFile(lg, p, n, st)
				_ = d.EditRemoteFile(lg, p, n, st)
			}
		}(path, name)
	}
	wg.Wait()
}
