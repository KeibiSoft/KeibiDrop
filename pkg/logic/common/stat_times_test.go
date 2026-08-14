// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests the statTimes platform helper: a Chtimes-set atime round-trips
// ABOUTME: through the announce path, and btime stays in a sane range.

package common

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestStatTimes_RoundTripsChtimes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
	atime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	mtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(p, atime, mtime))

	info, err := os.Stat(p)
	require.NoError(t, err)
	at, bt := statTimes(info)
	require.WithinDuration(t, atime, time.Unix(0, int64(at)), time.Second,
		"atime must round-trip through statTimes")
	// Birth time: real creation time where the platform has one (darwin,
	// windows); mtime fallback on linux. Either way a sane timestamp.
	require.NotZero(t, bt)
	got := time.Unix(0, int64(bt))
	require.True(t, got.After(time.Now().Add(-49*time.Hour)), "btime below sane range: %v", got)
	require.True(t, got.Before(time.Now().Add(time.Minute)), "btime in the future: %v", got)
}
