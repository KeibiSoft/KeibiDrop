//go:build windows

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package filesystem

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// flushSignal reports each refresher flush: "File manager refresh" is logged
// right before the flush runs.
type flushSignal struct{ c chan struct{} }

func (s *flushSignal) Enabled(context.Context, slog.Level) bool { return true }
func (s *flushSignal) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "File manager refresh" {
		select {
		case s.c <- struct{}{}:
		default:
		}
	}
	return nil
}
func (s *flushSignal) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *flushSignal) WithGroup(string) slog.Handler      { return s }

// An unmount while a flush of notifies runs. Destroy stops the refresher
// before WinFsp frees the volume, so no notify reaches a freed volume, and the
// unmount waits at most for the notify in progress, not the whole batch.
func TestUnmountWhileARefreshFlushRuns(t *testing.T) {
	for round := range 3 {
		flushing := make(chan struct{}, 1)
		fs := NewFS(slog.New(&flushSignal{c: flushing}))
		mnt := filepath.Join(t.TempDir(), "mnt")
		mounted := make(chan error, 1)
		go func() { mounted <- fs.Mount(mnt, false, t.TempDir()) }()
		testkit.Eventually(t, 15*time.Second, 50*time.Millisecond, func() bool {
			select {
			case err := <-mounted:
				t.Skipf("no WinFsp mount here: %v", err)
			default:
			}
			fi, err := os.Lstat(mnt)
			return err == nil && fi.Mode()&(os.ModeIrregular|os.ModeSymlink) != 0 && fs.IsMounted()
		}, "the mount")

		for i := range maxRefreshBatch + 100 {
			fs.PeerChanged(fmt.Sprintf("/r%d/f%04d.txt", round, i), PeerAdded)
		}
		if err := testkit.Within(10*time.Second, "a flush to start", func() error { <-flushing; return nil }); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		fs.Unmount()
		if err := testkit.Within(10*time.Second, "Mount to return after the unmount", func() error { return <-mounted }); err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("round %d: unmount during a flush took %s", round, took)
		}
	}
}
