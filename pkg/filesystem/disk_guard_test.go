// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file pins the free-space guard: under the floor no fetch starts and a
// read gets ENOSPC; the OS is asked once per second; the session hears each
// crossing once; a statfs error never blocks a read.

package filesystem

import (
	"context"
	"errors"
	"testing"
	"time"

	winfuse "github.com/winfsp/cgofuse/fuse"
)

// stubFreeSpace replaces the OS answer for the test and counts the asks.
func stubFreeSpace(t *testing.T, free *uint64, err error) *int {
	t.Helper()
	calls := new(int)
	orig := freeDiskSpace
	freeDiskSpace = func(string) (uint64, error) {
		*calls++
		return *free, err
	}
	t.Cleanup(func() { freeDiskSpace = orig })
	return calls
}

func TestDiskGuard_RefusesReadsUnderTheFloor(t *testing.T) {
	content := makePattern(2 * ChunkSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	free := uint64(LowDiskFloor - 1)
	calls := stubFreeSpace(t, &free, nil)
	var reports []bool
	root.SetOnLowDisk(func(low bool, _ uint64) { reports = append(reports, low) })

	buf := make([]byte, 4096)
	for i := 0; i < 2; i++ {
		if n := root.Read("/f.bin", buf, 0, fh); n != -winfuse.ENOSPC {
			t.Fatalf("read %d under the floor returned %d, want ENOSPC", i, n)
		}
	}
	if prov.reads.Load() != 0 {
		t.Fatalf("a fetch started with the disk under the floor")
	}
	if *calls != 1 {
		t.Fatalf("statfs asked %d times within the pace, want 1", *calls)
	}
	if len(reports) != 1 || !reports[0] {
		t.Fatalf("crossing reports = %v, want one low report", reports)
	}
	if !root.LowDisk() {
		t.Fatalf("LowDisk false while under the floor")
	}

	// Space at the floor is not enough to open again; a block above it is.
	free = uint64(LowDiskFloor)
	root.disk.checked = time.Time{}
	if n := root.Read("/f.bin", buf, 0, fh); n != -winfuse.ENOSPC {
		t.Fatalf("read at the floor returned %d, want ENOSPC (hysteresis)", n)
	}
	free = uint64(lowDiskClear)
	root.disk.checked = time.Time{}
	if n := root.Read("/f.bin", buf, 0, fh); n != len(buf) {
		t.Fatalf("read with space back returned %d, want %d", n, len(buf))
	}
	if prov.reads.Load() != 1 {
		t.Fatalf("fetches after space came back = %d, want 1", prov.reads.Load())
	}
	if len(reports) != 2 || reports[1] {
		t.Fatalf("crossing reports = %v, want low then clear", reports)
	}
	if root.LowDisk() {
		t.Fatalf("LowDisk true after space came back")
	}
}

func TestDiskGuard_StatfsErrorAllowsTheRead(t *testing.T) {
	content := makePattern(2 * ChunkSize)
	root, fh, prov, cleanup := newReadAheadFile(t, content)
	defer cleanup()
	free := uint64(0)
	stubFreeSpace(t, &free, errors.New("statfs: not supported"))
	reported := false
	root.SetOnLowDisk(func(bool, uint64) { reported = true })

	buf := make([]byte, 4096)
	if n := root.Read("/f.bin", buf, 0, fh); n != len(buf) {
		t.Fatalf("read with a failing statfs returned %d, want %d", n, len(buf))
	}
	if prov.reads.Load() != 1 || reported || root.LowDisk() {
		t.Fatalf("a failing statfs changed the guard: fetches %d reported %v low %v", prov.reads.Load(), reported, root.LowDisk())
	}
}

// The read-ahead window fetches nothing under the floor and registers no unit.
func TestDiskGuard_WindowStopsUnderTheFloor(t *testing.T) {
	fileSize := 2 * ReadAheadBlock
	root, fh, prov, cleanup := newReadAheadFile(t, makePattern(fileSize))
	defer cleanup()
	free := uint64(LowDiskFloor - 1)
	stubFreeSpace(t, &free, nil)
	f := root.OpenFileHandlers[fh].File
	pool := raPool(t, prov, fh)
	defer func() { _ = pool.Close() }()

	f.CacheWg.Add(1)
	root.prefetchRange(f, pool, f.CacheFD, f.Bitmap, 0, 2, int64(fileSize), context.Background())
	f.CacheWg.Wait()
	if prov.reads.Load() != 0 {
		t.Fatalf("the window fetched %d blocks under the floor", prov.reads.Load())
	}
	if !inflightEmpty(f) {
		t.Fatalf("the window left a unit registered")
	}
}
