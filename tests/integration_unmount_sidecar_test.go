// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Unmount writes every sidecar still on its 1 s timer: nothing writes into
// ABOUTME: the save folder after Unmount returns, and a quit loses no landing.

package tests

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// sidecarStamps maps each .kdbitmap under dir to its mtime.
func sidecarStamps(t *testing.T, dir string) map[string]time.Time {
	t.Helper()
	out := make(map[string]time.Time)
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(p, ".kdbitmap") {
			return nil
		}
		if info, err := e.Info(); err == nil {
			rel, _ := filepath.Rel(dir, p)
			out[rel] = info.ModTime()
		}
		return nil
	})
	return out
}

// One cold read warms the siblings in a batch. Their landings arm the 1 s
// sidecar timers and no handle is open to flush them. Before the fix those
// timers fired after the unmount: TempDir cleanup on Windows then failed with
// "The directory is not empty" (TestUXLatency_ManySmallFiles, 4 Oct), and a
// quit right after the unmount lost them.
func TestUnmount_FlushesWarmedSidecars(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)
	// The harness prefetches on open; every Windows stat opens the file, and a
	// completed prefetch drops its sidecar by design. The warm must land them.
	t.Setenv("KEIBIDROP_PREFETCH_ON_OPEN", "0")

	const count = 20
	const size = 15 * 1024
	var names []string
	for i := range count {
		names = append(names, fmt.Sprintf("warm_%02d.bin", i))
	}
	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			dir := filepath.Join(bobSave, "warm")
			require.NoError(os.MkdirAll(dir, 0o755))
			for i, name := range names {
				require.NoError(os.WriteFile(filepath.Join(dir, name), makeTestPattern(size+i), 0o644))
			}
			bob.ScanSharedOnStart = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)
	for i, name := range names {
		p := filepath.Join(tp.AliceMountDir, "warm", name)
		WaitForCondition(t, 30*time.Second, 50*time.Millisecond, func() bool {
			info, err := os.Stat(p)
			return err == nil && info.Size() == int64(size+i)
		}, "waiting for "+name)
	}

	_, err := os.ReadFile(filepath.Join(tp.AliceMountDir, "warm", names[0]))
	require.NoError(err)
	root := tp.Alice.FS.Root()
	require.NotNil(root)
	testkit.Eventually(t, 30*time.Second, 5*time.Millisecond, func() bool {
		for _, name := range names[1:] {
			root.RemoteFilesLock.RLock()
			f := root.RemoteFiles["/warm/"+name]
			root.RemoteFilesLock.RUnlock()
			if f == nil || f.Bitmap == nil || !f.Bitmap.IsComplete() {
				return false
			}
		}
		return true
	}, "the warm batch to complete the siblings")

	tp.Alice.FS.Unmount()

	after := sidecarStamps(t, tp.AliceSaveDir)
	for _, name := range names[1:] {
		_, ok := after[filepath.Join("warm", name)+".kdbitmap"]
		require.Truef(ok, "the sidecar of warmed %s must be on disk when Unmount returns", name)
	}
	time.Sleep(1500 * time.Millisecond)
	require.Equal(after, sidecarStamps(t, tp.AliceSaveDir), "a sidecar was written after Unmount returned")
}
