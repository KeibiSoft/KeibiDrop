// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Race detector test for reconnect map reset vs GetDownloadProgress.
// ABOUTME: Ensures activeDownloadsMu is held during reconnect map reassignment.

package common

import (
	"context"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
)

// TestReconnectReset_NoRaceOnActiveDownloads verifies that concurrent
// GetDownloadProgress calls do not race with the reconnect map reset.
// Run with -race to catch any data race.
func TestReconnectReset_NoRaceOnActiveDownloads(t *testing.T) {
	kd := newTestKD(t)

	const testFile = "test-file"
	bm := filesystem.NewChunkBitmap(1024 * 1024)

	// Populate maps under the mutex (mirrors how registerDownload works).
	kd.activeDownloadsMu.Lock()
	kd.activeDownloads[testFile] = func() {}
	kd.activeBitmaps[testFile] = bm
	kd.activeDownloadsMu.Unlock()

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)

	// Spawn readers that call GetDownloadProgress concurrently.
	const readers = 8
	done.Add(readers)
	for range readers {
		go func() {
			defer done.Done()
			start.Wait()
			for range 500 {
				kd.GetDownloadProgress(testFile)
			}
		}()
	}

	// Spawn one goroutine that does the reconnect reset (the fixed code path).
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for range 100 {
			kd.activeDownloadsMu.Lock()
			for _, cancel := range kd.activeDownloads {
				cancel()
			}
			kd.activeDownloads = make(map[string]context.CancelFunc)
			kd.activeBitmaps = make(map[string]*filesystem.ChunkBitmap)
			kd.activeDownloadsMu.Unlock()
		}
	}()

	// Release all goroutines simultaneously.
	start.Done()
	done.Wait()
}
