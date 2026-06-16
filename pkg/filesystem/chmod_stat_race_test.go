// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Mount-free -race guard: Chmod/Chown write f.stat.Mode under a map lock
// ABOUTME: while OpenEx reads it (cacheMode) under metaMu on the same *File.

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

// TestChmodStatModeRace drives Chmod and Chown (which write f.stat.Mode/.Uid/.Gid
// on the AllFileMap/RemoteFiles entry) concurrently with OpenEx, which reads
// fh.stat.Mode (cacheMode) under metaMu, on the SAME *File (the test aliases the
// AddRemoteFile entry into AllFileMap too). Before the fix Chmod/Chown wrote f.stat
// under the map lock only, a different lock than the metaMu readers, so -race
// flagged the .Mode access. Getattr is the control: it holds AfmLock across its
// whole body, so it is excluded from Chmod/Chown and must never appear in a race
// report. Mount-free, so it cannot trip the unrelated cgofuse teardown race.
func TestChmodStatModeRace(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.OpenStreamProvider = func() types.FileStreamProvider { return nil }
	d.OnLocalChange = nil
	lg := nopLogger()

	const numFiles = 8
	const iters = 400
	var wg sync.WaitGroup

	for i := 0; i < numFiles; i++ {
		path := fmt.Sprintf("/m_%04d", i)
		name := fmt.Sprintf("m_%04d", i)
		st := &winfuse.Stat_t{Size: 4096, Mode: 0o644 | winfuse.S_IFREG}
		st.Mtim.Sec = 1
		_ = d.AddRemoteFile(lg, path, name, st)

		// Alias the same *File into AllFileMap so Chmod/Chown hit it under AfmLock.
		d.RemoteFilesLock.RLock()
		f := d.RemoteFiles[path]
		d.RemoteFilesLock.RUnlock()
		if f == nil {
			t.Fatalf("no remote file for %s", path)
		}
		d.AfmLock.Lock()
		d.AllFileMap[path] = f
		d.AfmLock.Unlock()
		_ = os.WriteFile(filepath.Join(d.LocalDownloadFolder, name), make([]byte, 4096), 0644)

		wg.Add(4)
		go func(p string) { // Chmod: writes f.stat.Mode
			defer wg.Done()
			for k := 0; k < iters; k++ {
				d.Chmod(p, 0o600|uint32(k&7))
			}
		}(path)
		go func(p string) { // Chown: writes f.stat.Uid/.Gid/.Mode
			defer wg.Done()
			for k := 0; k < iters; k++ {
				d.Chown(p, uint32(k&3), uint32(k&3))
			}
		}(path)
		go func(p string) { // OpenEx: reads fh.stat.Mode (cacheMode) under metaMu
			defer wg.Done()
			for k := 0; k < iters; k++ {
				fi := &winfuse.FileInfo_t{Flags: syscall.O_RDONLY}
				d.OpenEx(p, fi)
				if fi.Fh != 0 {
					d.Release(p, fi.Fh)
				}
			}
		}(path)
		go func(p string) { // Getattr: control (AfmLock-excluded), must stay clean
			defer wg.Done()
			for k := 0; k < iters; k++ {
				var s winfuse.Stat_t
				d.Getattr(p, &s, 0)
			}
		}(path)
	}
	wg.Wait()
}
