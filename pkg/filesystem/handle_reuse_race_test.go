// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

package filesystem

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/stretchr/testify/require"
)

// TestHandleReuseRace exercises the fd-recycling race that caused 50% data loss
// during high-throughput file creation (e.g., PostgreSQL initdb creating 224 files
// in ~1 second). Before the opaque-handle fix, the kernel would recycle fd numbers
// after close(), causing OpenFileHandlers map collisions and cross-file writes.
func TestHandleReuseRace(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.OnLocalChange = func(event types.FileEvent) {}
	d.OpenStreamProvider = func() types.FileStreamProvider { return nil }

	const numFiles = 250
	errs := make([]error, numFiles)
	var wg sync.WaitGroup

	for i := 0; i < numFiles; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			filename := fmt.Sprintf("/file_%04d.dat", idx)
			content := []byte(fmt.Sprintf("content-for-file-%04d-padding-to-increase-size", idx))

			fi := &winfuse.FileInfo_t{}
			errCode := d.CreateEx(filename, 0644, fi)
			if errCode != 0 {
				errs[idx] = fmt.Errorf("CreateEx failed with code %d", errCode)
				return
			}

			n := d.Write(filename, content, 0, fi.Fh)
			if n != len(content) {
				errs[idx] = fmt.Errorf("Write returned %d, want %d", n, len(content))
				return
			}

			errCode = d.Release(filename, fi.Fh)
			if errCode != 0 {
				errs[idx] = fmt.Errorf("Release failed with code %d", errCode)
				return
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < numFiles; i++ {
		require.NoError(t, errs[i], "file %d lifecycle error", i)
	}

	// Verify every file has the correct content (no cross-contamination)
	for i := 0; i < numFiles; i++ {
		filename := fmt.Sprintf("file_%04d.dat", i)
		expected := fmt.Sprintf("content-for-file-%04d-padding-to-increase-size", i)
		got, err := os.ReadFile(filepath.Join(saveDir, filename))
		require.NoError(t, err, "reading file %d", i)
		require.Equal(t, expected, string(got),
			"file %d has wrong content (cross-contamination from fd reuse)", i)
	}
}

// TestHandleIDsAreUnique verifies that opaque handle IDs are never reused,
// even when files are created, released, and re-created rapidly.
func TestHandleIDsAreUnique(t *testing.T) {
	saveDir := t.TempDir()
	d := newTestDir(saveDir)
	d.OnLocalChange = func(event types.FileEvent) {}
	d.OpenStreamProvider = func() types.FileStreamProvider { return nil }

	seen := make(map[uint64]string)
	var mu sync.Mutex

	const numRounds = 100
	for round := 0; round < numRounds; round++ {
		filename := fmt.Sprintf("/round_%04d.dat", round)
		fi := &winfuse.FileInfo_t{}
		errCode := d.CreateEx(filename, 0644, fi)
		require.Equal(t, 0, errCode, "CreateEx round %d", round)

		mu.Lock()
		prev, dup := seen[fi.Fh]
		seen[fi.Fh] = filename
		mu.Unlock()
		require.False(t, dup, "handle ID %d reused: first=%s, now=%s", fi.Fh, prev, filename)

		d.Release(filename, fi.Fh)
	}
}
