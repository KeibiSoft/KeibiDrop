// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Past the cap the file becomes .1 and a fresh one starts; only one .1 is kept
// and a record is never split across the two.
func TestRotatingLog_RotatesPastTheCapAndKeepsOnePrevious(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keibidrop.log")
	r, err := openRotatingLog(path, 64)
	require.NoError(t, err)
	defer r.Close()

	one := strings.Repeat("1", 40)
	two := strings.Repeat("2", 40)
	three := strings.Repeat("3", 40)
	for _, rec := range []string{one, two, three} {
		n, err := r.Write([]byte(rec))
		require.NoError(t, err)
		require.Equal(t, len(rec), n)
	}

	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, three, string(cur), "the current file holds only the record that started it")
	prev, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, two, string(prev), "one previous generation, the latest one")
	_, err = os.Stat(path + ".2")
	require.True(t, os.IsNotExist(err), "no third generation")
}

// A reopened log counts what is already in the file, so a restart does not
// reset the cap.
func TestRotatingLog_ReopenCountsTheExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keibidrop.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 40)), 0o644))
	r, err := openRotatingLog(path, 64)
	require.NoError(t, err)
	defer r.Close()
	_, err = r.Write([]byte(strings.Repeat("b", 40)))
	require.NoError(t, err)
	cur, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("b", 40), string(cur))
	prev, err := os.ReadFile(path + ".1")
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", 40), string(prev))
}

// OpenLogFile appends, as every binary expects, and creates the file.
func TestOpenLogFile_AppendsAndCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keibidrop.log")
	w, err := OpenLogFile(path)
	require.NoError(t, err)
	_, err = w.Write([]byte("first\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	w, err = OpenLogFile(path)
	require.NoError(t, err)
	_, err = w.Write([]byte("second\n"))
	require.NoError(t, err)
	require.NoError(t, w.Close())
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "first\nsecond\n", string(got))
}

// An instance with its own config dir logs there. Two instances on one machine
// without an explicit log_file used to append to the one platform file.
func TestDefaultConfig_LogFileFollowsTheConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KEIBIDROP_CONFIG_DIR", dir)
	require.Equal(t, filepath.Join(dir, "keibidrop.log"), DefaultConfig().LogFile)

	t.Setenv("KEIBIDROP_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	want := filepath.Join(home, ".local", "share", "keibidrop", "keibidrop.log")
	if runtime.GOOS == "darwin" {
		want = filepath.Join(home, "Library", "Logs", "KeibiDrop", "keibidrop.log")
	}
	require.Equal(t, want, DefaultConfig().LogFile)
}
