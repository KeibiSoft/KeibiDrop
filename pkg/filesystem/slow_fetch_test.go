// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"sync/atomic"
	"testing"
	"time"
)

// The slow-fetch report reaches the session through the root, only for a
// wait past SlowFetchNotice, and carries the wait so the session can log it.
func TestSlowFetch_ReportedThroughTheRootPastTheThreshold(t *testing.T) {
	const blocks = 2
	fileSize := blocks * ReadAheadBlock
	content := makePattern(fileSize)

	run := func(delay time.Duration) (reports int32, waited time.Duration) {
		prov := &delayProvider{content: content, delay: delay}
		root, fh, cleanup := newReadAheadFileWith(t, int64(len(content)), prov)
		defer cleanup()
		root.ReadAheadWindowBlocks = 4 // probe units, not whole blocks
		var n atomic.Int32
		var last atomic.Int64
		root.SetOnSlowFetch(func(w time.Duration) { n.Add(1); last.Store(int64(w)) })
		got := make([]byte, 4096)
		if r := root.Read("/f.bin", got, ReadAheadBlock, fh); r != len(got) {
			t.Fatalf("read %d bytes, want %d", r, len(got))
		}
		return n.Load(), time.Duration(last.Load())
	}

	// A probe unit is an eighth of a block, floored at a fifth of the delay:
	// 100 ms of block time is a 20 ms wait, well under the threshold.
	if n, _ := run(100 * time.Millisecond); n != 0 {
		t.Fatalf("a fast fetch was reported %d times, want 0", n)
	}
	// 12 s of block time is a 2.4 s wait for the unit: past the threshold.
	n, waited := run(12 * time.Second)
	if n != 1 {
		t.Fatalf("a slow fetch was reported %d times, want 1", n)
	}
	if waited <= SlowFetchNotice {
		t.Fatalf("reported wait %v, want more than %v", waited, SlowFetchNotice)
	}
}

// With no session listening the report is a no-op, and a child routes to the
// root like the other callbacks.
func TestSlowFetch_NoListenerIsQuiet(t *testing.T) {
	root := NewBareRoot(t.TempDir())
	root.noteSlowFetch(5 * time.Second) // must not panic
	child := &Dir{Root: root}
	var n atomic.Int32
	root.SetOnSlowFetch(func(time.Duration) { n.Add(1) })
	child.noteSlowFetch(5 * time.Second)
	if n.Load() != 1 {
		t.Fatalf("child report reached the root %d times, want 1", n.Load())
	}
	root.SetOnSlowFetch(nil)
	child.noteSlowFetch(5 * time.Second)
	if n.Load() != 1 {
		t.Fatal("a cleared listener still received a report")
	}
}
