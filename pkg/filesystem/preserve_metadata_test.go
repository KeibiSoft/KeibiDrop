// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests ApplyOriginMetadata and the origin-capture path: mode and
// ABOUTME: times land on disk, and captured values survive Getattr pollution.

package filesystem

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

func TestApplyOriginMetadata_SetsModeAndMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	require.NoError(t, os.WriteFile(p, []byte("data"), 0o666))

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	require.NoError(t, ApplyOriginMetadata(p, 0o100640, origAtime.UnixNano(), origMtime.UnixNano()))

	info, err := os.Stat(p)
	require.NoError(t, err)
	require.WithinDuration(t, origMtime, info.ModTime(), time.Second)
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	}
}

func TestApplyOriginMetadata_ZeroMtimeIsNoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	require.NoError(t, os.WriteFile(p, []byte("data"), 0o644))

	require.NoError(t, ApplyOriginMetadata(p, 0o400, 0, 0))

	info, err := os.Stat(p)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now(), info.ModTime(), time.Minute, "no origin data: times untouched")
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "no origin data: mode untouched")
	}
}

func TestApplyOriginMetadata_ZeroAtimeFallsBackToMtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	require.NoError(t, os.WriteFile(p, []byte("data"), 0o644))

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	require.NoError(t, ApplyOriginMetadata(p, 0, 0, origMtime.UnixNano()))

	info, err := os.Stat(p)
	require.NoError(t, err)
	require.WithinDuration(t, origMtime, info.ModTime(), time.Second)
}

// TestCaptureApplyOriginMeta_SurvivesStatPollution reproduces the download
// race: Getattr adopts the local partial file's stat mid-download, so f.stat
// no longer holds the origin's times at completion. The captured origin
// fields must still drive the apply, and realign the view.
func TestCaptureApplyOriginMeta_SurvivesStatPollution(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	require.NoError(t, os.WriteFile(p, []byte("data"), 0o666))

	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	announce := &winfuse.Stat_t{
		Mode: 0o100640,
		Size: 4,
		Atim: winfuse.NewTimespec(origAtime),
		Mtim: winfuse.NewTimespec(origMtime),
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	f := &File{logger: logger, stat: announce}
	captureOriginMeta(f, announce)

	// Getattr-style pollution: the partial file on disk is newer.
	f.stat.Mtim = winfuse.NewTimespec(time.Now())
	f.stat.Atim = winfuse.NewTimespec(time.Now())

	f.metaMu.Lock()
	applyOriginMetaLocked(f, p)
	f.metaMu.Unlock()

	info, err := os.Stat(p)
	require.NoError(t, err)
	require.WithinDuration(t, origMtime, info.ModTime(), time.Second, "disk gets the origin mtime")
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o640), info.Mode().Perm(), "disk gets the origin mode")
	}
	require.Equal(t, origMtime.UnixNano(), f.stat.Mtim.Sec*1e9+f.stat.Mtim.Nsec, "view realigned to origin mtime")
	require.Equal(t, origAtime.UnixNano(), f.stat.Atim.Sec*1e9+f.stat.Atim.Nsec, "view realigned to origin atime")
}
