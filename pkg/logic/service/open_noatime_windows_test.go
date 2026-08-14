// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Proves a no-atime serve read leaves the file's access time alone
// ABOUTME: on Windows (handle sentinel; also holds when the volume disables it).

//go:build windows

package service

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenReadNoAtime_DoesNotBumpAtime(t *testing.T) {
	p := filepath.Join(t.TempDir(), "evidence.bin")
	require.NoError(t, os.WriteFile(p, []byte("evidence"), 0o600))
	old := time.Now().Add(-30 * time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(p, old, old))

	f, err := openReadNoAtime(p)
	require.NoError(t, err)
	buf := make([]byte, 8)
	_, err = f.Read(buf)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	info, err := os.Stat(p)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Win32FileAttributeData)
	require.True(t, ok)
	got := time.Unix(0, st.LastAccessTime.Nanoseconds())
	require.WithinDuration(t, old, got, time.Second, "serve read must not change atime")
}
