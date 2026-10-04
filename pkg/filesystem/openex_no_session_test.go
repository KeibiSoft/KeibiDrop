//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// A session drop keeps the mount and the peer's files (CancelInFlight keeps
// the maps). An open that wanted the peer's bytes then had no stream
// provider: OpenEx passed nil to NewStreamPool, and the nil-pointer panic came
// back as EIO. Every Windows stat opens the file, so after a drop a peer file
// failed to stat (TestReconnectConflict_OfflineEditsBothSides, Windows full
// suite, 4 Oct).

package filesystem

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// panicSpy counts the panics recoverPanic turns into EIO.
type panicSpy struct{ panics atomic.Int32 }

func (s *panicSpy) Enabled(context.Context, slog.Level) bool { return true }
func (s *panicSpy) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "PANIC in FUSE handler" {
		s.panics.Add(1)
	}
	return nil
}
func (s *panicSpy) WithAttrs([]slog.Attr) slog.Handler { return s }
func (s *panicSpy) WithGroup(string) slog.Handler      { return s }

func dropSession(d *Dir) {
	d.SetStreamProvider(func() types.FileStreamProvider { return nil })
}

func TestOpenEx_AfterASessionDropRefusesAnIncompleteCopy(t *testing.T) {
	const size = int64(64 * 1024)
	d, _, _, _ := newFillDir(t, size)
	spy := &panicSpy{}
	d.logger = slog.New(spy)
	dropSession(d)

	fi := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, -winfuse.EIO, d.OpenEx("/big.bin", fi), "no session can bring the bytes")
	require.Zero(t, spy.panics.Load(), "the refusal must be a decision, not a recovered panic")
}

func TestOpenEx_AfterASessionDropServesACompleteCopy(t *testing.T) {
	const size = int64(64 * 1024)
	d, _, _, _ := newFillDir(t, size)
	spy := &panicSpy{}
	d.logger = slog.New(spy)
	want := make([]byte, size)
	for i := range want {
		want[i] = fillByte(int64(i))
	}

	// Read it whole while the session is up.
	fi := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, 0, d.OpenEx("/big.bin", fi))
	buf := make([]byte, size)
	require.Equal(t, int(size), d.Read("/big.bin", buf, 0, fi.Fh))
	require.Equal(t, want, buf)
	require.Equal(t, 0, d.Release("/big.bin", fi.Fh))
	d.RemoteFilesLock.RLock()
	f := d.RemoteFiles["/big.bin"]
	d.RemoteFilesLock.RUnlock()
	f.metaMu.RLock()
	bm := f.Bitmap
	f.metaMu.RUnlock()
	require.True(t, bm.IsComplete(), "precondition: the read landed every chunk")

	dropSession(d)

	fi = &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, 0, d.OpenEx("/big.bin", fi), "a complete copy must open without a session")
	got := make([]byte, size)
	require.Equal(t, int(size), d.Read("/big.bin", got, 0, fi.Fh))
	require.Equal(t, want, got)
	require.Equal(t, 0, d.Release("/big.bin", fi.Fh))
	require.Zero(t, spy.panics.Load())
}
