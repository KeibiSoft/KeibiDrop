//go:build !android

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

type flushLog struct {
	mu      sync.Mutex
	batches []map[string]PeerChange
	at      []time.Time
}

func (l *flushLog) flush(_ context.Context, b map[string]PeerChange) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.batches = append(l.batches, b)
	l.at = append(l.at, time.Now())
}

func (l *flushLog) snapshot() ([]map[string]PeerChange, []time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]PeerChange(nil), l.batches...), append([]time.Time(nil), l.at...)
}

func startTestRefresher(t *testing.T, interval time.Duration) (*fileManagerRefresher, *flushLog, context.CancelFunc) {
	t.Helper()
	l := &flushLog{}
	r := newFileManagerRefresher(l.flush)
	r.interval = interval
	ctx, cancel := context.WithCancel(context.Background())
	go r.run(ctx)
	t.Cleanup(cancel)
	return r, l, cancel
}

// A burst of 1000 announces (plus their folder) is two flushes (512 + 489),
// an interval apart, each path exactly once: never one refresh per file.
func TestRefresherBatchesABurst(t *testing.T) {
	const interval = 100 * time.Millisecond
	r, l, _ := startTestRefresher(t, interval)
	start := time.Now()
	for i := 0; i < 1000; i++ {
		r.add(fmt.Sprintf("dir/f%04d", i), PeerAdded)
	}
	time.Sleep(5 * interval)
	batches, at := l.snapshot()
	if len(batches) != 2 {
		t.Fatalf("flushes = %d, want 2", len(batches))
	}
	seen := map[string]bool{}
	for _, b := range batches {
		if len(b) > maxRefreshBatch {
			t.Fatalf("a flush carried %d paths, cap %d", len(b), maxRefreshBatch)
		}
		for p := range b {
			if seen[p] {
				t.Fatalf("%s flushed twice", p)
			}
			seen[p] = true
		}
	}
	if len(seen) != 1001 || !seen["/dir/f0000"] || !seen["/dir"] {
		t.Fatalf("flushed %d distinct paths (want 1000 files and /dir, keyed with a leading /)", len(seen))
	}
	if at[0].Sub(start) < interval || at[1].Sub(at[0]) < interval {
		t.Fatalf("flushes not an interval apart: first after %v, second %v later", at[0].Sub(start), at[1].Sub(at[0]))
	}
}

// A path changed twice before the flush is reported once, with its last change.
func TestRefresherLastChangeWins(t *testing.T) {
	r, l, _ := startTestRefresher(t, 50*time.Millisecond)
	r.add("/a.txt", PeerAdded)
	r.add("/a.txt", PeerRemoved)
	time.Sleep(250 * time.Millisecond)
	batches, _ := l.snapshot()
	if len(batches) != 1 || len(batches[0]) != 1 || batches[0]["/a.txt"] != PeerRemoved {
		t.Fatalf("batches = %v, want one flush of /a.txt removed", batches)
	}
}

// A deep add reports the folders it made on the way; an edit does not.
func TestRefresherReportsTheFoldersADeepAddMade(t *testing.T) {
	r, l, _ := startTestRefresher(t, 50*time.Millisecond)
	r.add("drop/sub/b.bin", PeerAdded)
	r.add("/other/e.txt", PeerEdited)
	time.Sleep(250 * time.Millisecond)
	batches, _ := l.snapshot()
	want := map[string]PeerChange{"/drop/sub/b.bin": PeerAdded, "/drop/sub": PeerDirAdded, "/drop": PeerDirAdded, "/other/e.txt": PeerEdited}
	if len(batches) != 1 || fmt.Sprint(batches[0]) != fmt.Sprint(want) {
		t.Fatalf("batches = %v, want one flush of %v", batches, want)
	}
}

// A burst under one folder names that folder once, not in every flush; after
// the peer removes it, a new file under it names it again.
func TestRefresherNamesAFolderOnce(t *testing.T) {
	r, l, _ := startTestRefresher(t, 50*time.Millisecond)
	count := func() (n int) {
		batches, _ := l.snapshot()
		for _, b := range batches {
			if _, ok := b["/burst"]; ok {
				n++
			}
		}
		return n
	}
	r.add("/burst/f1", PeerAdded)
	time.Sleep(150 * time.Millisecond)
	r.add("/burst/f2", PeerAdded)
	time.Sleep(150 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("/burst named in %d flushes, want 1", n)
	}
	r.add("/burst", PeerDirRemoved)
	r.add("/burst/f3", PeerAdded)
	time.Sleep(150 * time.Millisecond)
	batches, _ := l.snapshot()
	if n := count(); n != 2 || batches[len(batches)-1]["/burst"] != PeerDirAdded {
		t.Fatalf("after a remove and a new file: /burst named %d times, last %v; want 2, added", n, batches[len(batches)-1]["/burst"])
	}
}

// Nothing flushes after the mount's context ends.
func TestRefresherStopsWithItsContext(t *testing.T) {
	r, l, cancel := startTestRefresher(t, 50*time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)
	r.add("/late.txt", PeerAdded)
	time.Sleep(200 * time.Millisecond)
	if batches, _ := l.snapshot(); len(batches) != 0 {
		t.Fatalf("flushed %v after the context ended", batches)
	}
	if err := testkit.Within(time.Second, "the refresher goroutine to end", func() error { <-r.done; return nil }); err != nil {
		t.Fatal(err)
	}
}

// A stop waits for the flush in progress, and that flush sees the stop. The
// Windows unmount needs both: WinFsp frees the volume after Destroy returns.
func TestRefresherStopWaitsForTheFlushInProgress(t *testing.T) {
	entered := make(chan struct{}, 1)
	var sawStop atomic.Bool
	r := newFileManagerRefresher(func(ctx context.Context, _ map[string]PeerChange) {
		entered <- struct{}{}
		<-ctx.Done() // a notify loop checks ctx between paths
		sawStop.Store(true)
	})
	r.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.run(ctx)
	r.add("/a.txt", PeerAdded)
	if err := testkit.Within(2*time.Second, "a flush to start", func() error { <-entered; return nil }); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := testkit.Within(time.Second, "the refresher to stop", func() error { <-r.done; return nil }); err != nil {
		t.Fatal(err)
	}
	if !sawStop.Load() {
		t.Fatal("run returned before the flush in progress ended")
	}
}

// However many folders a peer makes, the set of folders already named stays
// bounded for the life of the mount.
func TestRefresherBoundsTheFoldersItRemembers(t *testing.T) {
	r := newFileManagerRefresher(func(context.Context, map[string]PeerChange) {})
	for i := range maxKnownDirs + 100 {
		r.add(fmt.Sprintf("/d%05d/f.txt", i), PeerAdded)
	}
	r.mu.Lock()
	n := len(r.known)
	r.mu.Unlock()
	if n > maxKnownDirs {
		t.Fatalf("remembers %d folders, bound is %d", n, maxKnownDirs)
	}
}

func TestDestroyRunsItsHook(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.Destroy() // no hook set: nothing runs
	var ran atomic.Bool
	d.SetOnDestroy(func() { ran.Store(true) })
	d.Destroy()
	if !ran.Load() {
		t.Fatal("Destroy did not run its hook")
	}
}

// Only Windows (WinFsp notify) and Linux (Nautilus over D-Bus) refresh a file
// manager. Elsewhere the mount starts no refresher at all.
func TestPlatformRefreshOnlyWhereAFileManagerListens(t *testing.T) {
	has := newTestFS().platformRefresh(t.TempDir()) != nil
	if want := runtime.GOOS == "windows" || runtime.GOOS == "linux"; has != want {
		t.Fatalf("platformRefresh on %s: got a refresh %v, want %v", runtime.GOOS, has, want)
	}
}

func TestPeerChangedWithoutMountIsANoop(t *testing.T) {
	(&FS{}).PeerChanged("/x", PeerAdded)
}

func TestChangedDirs(t *testing.T) {
	m := filepath.FromSlash("/mnt/kd")
	got := changedDirs(m, map[string]PeerChange{
		"/top.txt": PeerAdded, "/docs/a.md": PeerEdited, "/docs/b.md": PeerRemoved, "/docs/inner": PeerDirAdded,
	})
	want := []string{m, filepath.Join(m, "docs")}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("changedDirs = %v, want %v", got, want)
	}
	many := map[string]PeerChange{}
	for i := 0; i < 100; i++ {
		many[fmt.Sprintf("/d%03d/f", i)] = PeerAdded
	}
	if n := len(changedDirs(m, many)); n != maxRefreshDirs {
		t.Fatalf("changedDirs kept %d folders, cap %d", n, maxRefreshDirs)
	}
}
