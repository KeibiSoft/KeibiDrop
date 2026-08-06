// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Mount-free -race guard for the Release async-notify data race.
// ABOUTME: Drives CreateEx/OpenEx/Release directly so it never mounts FUSE.

package filesystem

import (
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// TestReleaseAsyncNotifyRace reproduces the data race on a File's
// NotRemoteSynced/HadEdits sync-state bools. A file created without any FUSE
// Write takes the size-stabilization branch in Release, which spawns a
// long-lived goroutine that (pre-fix) cleared those two bools holding no lock.
// Closing, reopening, and closing the same path again spawns several such
// goroutines on the SAME *File; they all wake ~1.7s later and write the bools
// concurrently. The fix brings every access under metaMu (the File's innermost
// lock), so this is green with the fix and fails under -race without it.
//
// It runs entirely in-process (no host.Mount), so it cannot trip the unrelated
// pre-existing cgofuse teardown race that an end-to-end FUSE test hits under
// -race. That keeps this guard deterministic and cheap for CI.
func TestReleaseAsyncNotifyRace(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.SetStreamProvider(func() types.FileStreamProvider { return nil })

	const numFiles = 16
	const reopenCycles = 4 // func2 goroutines per file = reopenCycles + 1

	// Count surfaced files so the test waits for the racy window (all func2
	// reaching the bool write) before returning, and verifies they all notify.
	var mu sync.Mutex
	fired := make(map[string]int)
	d.SetOnLocalChange(func(ev types.FileEvent) {
		mu.Lock()
		fired[ev.Path]++
		mu.Unlock()
	})

	var wg sync.WaitGroup
	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			path := fmt.Sprintf("/empty_%04d", idx)

			fi := &winfuse.FileInfo_t{}
			if rc := d.CreateEx(path, 0644, fi); rc != 0 {
				t.Errorf("CreateEx %s: %d", path, rc)
				return
			}
			// No Write -> HadEdits stays false -> the func2 branch.
			d.Release(path, fi.Fh)

			for k := 0; k < reopenCycles; k++ {
				fo := &winfuse.FileInfo_t{Flags: syscall.O_RDWR}
				if rc := d.OpenEx(path, fo); rc != 0 {
					t.Errorf("OpenEx %s: %d", path, rc)
					return
				}
				d.Release(path, fo.Fh)
			}
		}(i)
	}
	wg.Wait()

	// Each file's func2 fires OnLocalChange ~1.7s after Release. Wait until
	// every file has surfaced at least once (the race, if present, has fired
	// by then) or fail.
	deadline := time.Now().Add(30 * time.Second)
	for {
		mu.Lock()
		distinct := len(fired)
		mu.Unlock()
		if distinct >= numFiles {
			break
		}
		if time.Now().After(deadline) {
			require.GreaterOrEqual(t, distinct, numFiles,
				"only %d/%d empty files surfaced via OnLocalChange", distinct, numFiles)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
