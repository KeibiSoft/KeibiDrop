// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Guards the Getattr/OpenEx/AddRemoteFile lock-order deadlock: OpenEx
// ABOUTME: must not take RemoteFilesLock while holding AfmLock.

package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// TestGetattrOpenAddNoDeadlock hammers Getattr, OpenEx, and AddRemoteFile on the
// same paths concurrently. Getattr holds RemoteFilesLock.RLock across Adm+AfmLock;
// OpenEx (not-in-AllFileMap branch) held AfmLock while taking RemoteFilesLock.RLock
// (inverted order); a pending AddRemoteFile RemoteFilesLock.Lock then closes a
// 3-way cycle under Go's RWMutex writer-preference. Before the fix this hangs;
// after OpenEx snapshots the remote-file check before AfmLock, it completes. A
// watchdog turns a hang into a fast, explicit failure with a goroutine dump.
func TestGetattrOpenAddNoDeadlock(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.SetStreamProvider(func() types.FileStreamProvider { return nil })
	lg := nopLogger()

	const numPaths = 8
	const iters = 400

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < numPaths; i++ {
			path := fmt.Sprintf("/d_%04d", i)
			name := fmt.Sprintf("d_%04d", i)
			// On-disk copy so Getattr lstat hits and OpenEx sees fileExists.
			_ = os.WriteFile(filepath.Join(saveDir, name), make([]byte, 64), 0644)

			wg.Add(3)
			go func(p string) { // Getattr storm
				defer wg.Done()
				for k := 0; k < iters; k++ {
					var gs winfuse.Stat_t
					d.Getattr(p, &gs, 0)
				}
			}(path)
			go func(p string) { // OpenEx (hits the not-in-map branch early)
				defer wg.Done()
				for k := 0; k < iters; k++ {
					fi := &winfuse.FileInfo_t{Flags: syscall.O_RDONLY}
					d.OpenEx(p, fi)
					if fi.Fh != 0 {
						d.Release(p, fi.Fh)
					}
				}
			}(path)
			go func(p, n string) { // AddRemoteFile: pending RemoteFilesLock writer
				defer wg.Done()
				for k := 0; k < iters; k++ {
					st := &winfuse.Stat_t{Size: int64(64 + k%3)}
					st.Mtim.Sec = int64(k + 1)
					_ = d.AddRemoteFile(lg, p, n, st)
				}
			}(path, name)
		}
		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(25 * time.Second):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("DEADLOCK: Getattr/OpenEx/AddRemoteFile did not finish in 25s\n%s", buf[:n])
	}
}
