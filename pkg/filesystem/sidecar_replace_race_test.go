// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A sidecar load must not fail while another writer renames a new version
// over it: the fill saves the sidecar from its goroutine while Release, a
// reopen or a restart loads it. On Windows a load without delete sharing hit
// ERROR_SHARING_VIOLATION (TestRestartMidFill_* failed 2 runs in 30, 4 Oct).
func TestLoadSidecarWhileAWriterReplacesIt(t *testing.T) {
	const size = 40 * 1048576
	path := BitmapPath(filepath.Join(t.TempDir(), "big.bin"))
	bm := NewChunkBitmap(size)
	if err := SaveSidecar(path, bm, nil); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = SaveSidecar(path, bm, nil)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()
	reads := 0
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); reads++ {
		if _, _, err := LoadSidecar(path, size); err != nil {
			t.Fatalf("load %d failed while a writer replaced the sidecar: %v", reads, err)
		}
	}
	t.Logf("%d loads, none failed", reads)
}
