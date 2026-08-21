// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Integration test for the sibling warmer over a real FUSE mount.
// ABOUTME: One cold read warms the directory's small files in one batch.

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// makeTestPattern returns n deterministic non-repeating bytes.
func makeTestPattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// TestSmallFileWarm_OneReadWarmsDirectory: an extraction-shaped tree (many
// small files, one large) on a scanned share. Reading ONE small file through
// the mount must warm every other small sibling into the cache in one batch,
// with origin mtimes applied, while the large file stays cold and the wire
// carries no duplicate payload.
func TestSmallFileWarm_OneReadWarmsDirectory(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	const smallCount = 40
	const smallSize = 15 * 1024
	origMtime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	content := make(map[string][]byte)
	var names []string
	for i := range smallCount {
		name := fmt.Sprintf("prefetch_%02d.pf", i)
		names = append(names, name)
		content[name] = makeTestPattern(smallSize + i)
	}
	largeContent := makeTestPattern(2 * 1024 * 1024)

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			dir := filepath.Join(bobSave, "evd")
			require.NoError(os.MkdirAll(dir, 0o755))
			for name, data := range content {
				p := filepath.Join(dir, name)
				require.NoError(os.WriteFile(p, data, 0o644))
				require.NoError(os.Chtimes(p, origMtime, origMtime))
			}
			largePath := filepath.Join(dir, "hive.dat")
			require.NoError(os.WriteFile(largePath, largeContent, 0o644))
			require.NoError(os.Chtimes(largePath, origMtime, origMtime))
			bob.ScanSharedOnStart = true
			alice.PreserveMetadata = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// Every file announced and visible before the trigger read.
	for _, name := range names {
		mountPath := filepath.Join(tp.AliceMountDir, "evd", name)
		WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
			info, err := os.Stat(mountPath)
			return err == nil && info.Size() == int64(len(content[name]))
		}, "waiting for "+name+" to be announced")
	}

	sentBefore, recvBefore := tp.Alice.WireStats()
	_ = sentBefore

	// The one cold read: the extraction walk's first file.
	trigger := names[0]
	got, err := os.ReadFile(filepath.Join(tp.AliceMountDir, "evd", trigger))
	require.NoError(err)
	require.Equal(content[trigger], got)

	// The warm batch completes every other small sibling without any
	// further reads.
	root := tp.Alice.FS.Root()
	require.NotNil(root)
	WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
		for _, name := range names[1:] {
			root.RemoteFilesLock.RLock()
			f := root.RemoteFiles["/evd/"+name]
			root.RemoteFilesLock.RUnlock()
			if f == nil || f.Bitmap == nil || !f.Bitmap.IsComplete() {
				return false
			}
		}
		return true
	}, "waiting for the warm batch to complete the siblings")

	// The large file stays cold: warming is for small files only.
	root.RemoteFilesLock.RLock()
	large := root.RemoteFiles["/evd/hive.dat"]
	root.RemoteFilesLock.RUnlock()
	require.NotNil(large)
	require.False(large.Bitmap.IsComplete(), "large file must not be warmed")

	// Warmed bytes are exact through the mount (served from cache now).
	for _, name := range names[1:] {
		got, err := os.ReadFile(filepath.Join(tp.AliceMountDir, "evd", name))
		require.NoError(err, name)
		require.Equal(content[name], got, "warmed bytes differ: %s", name)
	}

	// Origin mtime landed on the warmed disk copies (preserve_metadata).
	for _, name := range names[1:] {
		info, err := os.Stat(filepath.Join(tp.AliceSaveDir, "evd", name))
		require.NoError(err, name)
		require.WithinDuration(origMtime, info.ModTime(), time.Second,
			"warmed file must carry origin mtime: %s", name)
	}

	// No duplicate payload: everything the wire moved for the small files is
	// bounded by ~one copy of their bytes plus protocol overhead.
	var payload uint64
	for _, name := range names {
		payload += uint64(len(content[name]))
	}
	_, recvAfter := tp.Alice.WireStats()
	recvDelta := recvAfter - recvBefore
	require.Less(recvDelta, payload*3/2+128*1024,
		"wire recv %d exceeds 1.5x payload %d: duplicate fetching", recvDelta, payload)
	require.GreaterOrEqual(recvDelta, payload*9/10,
		"wire recv %d below payload %d: files did not actually travel", recvDelta, payload)
}
