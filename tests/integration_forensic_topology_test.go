// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Pins the forensic deployment: compromised machine serves evidence
// ABOUTME: read-only and untampered; the analyst gets bytes + origin metadata.

package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestForensicTopology_ReadOnlySourcePreservingAnalyst is the DFIR deployment
// as one test. Machine A (compromised, no FUSE needed): evidence in save_path,
// scan_shared_on_start announces it, share_read_only refuses every peer
// mutation. Machine B (analyst): FUSE mount with mount_read_only so local
// writes get EROFS, preserve_metadata so saved bytes keep origin timestamps.
// Invariants: B reads everything byte-exact with origin metadata; nothing on
// A changes (bytes, mtime, file set).
func TestForensicTopology_ReadOnlySourcePreservingAnalyst(t *testing.T) {
	skipIfNoFUSE(t)
	require := require.New(t)

	origMtime := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	origAtime := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	evidence := map[string][]byte{
		"dump/memory.raw": []byte("raw memory dump bytes"),
		"logs/auth.log":   []byte("sshd: accepted password for root"),
	}

	tp := SetupFUSEPeerPairPreConnect(t, 120*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			// Machine A: evidence collected into save_path before pairing.
			for rel, content := range evidence {
				p := filepath.Join(bobSave, filepath.FromSlash(rel))
				require.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(os.WriteFile(p, content, 0o644))
				require.NoError(os.Chmod(p, 0o640))
				require.NoError(os.Chtimes(p, origAtime, origMtime))
			}
			bob.ScanSharedOnStart = true
			bob.ShareReadOnly = true
			// Machine B: analyst.
			alice.MountReadOnly = true
			alice.PreserveMetadata = true
		})
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	// B gets every evidence file, byte-exact, through the mount.
	for rel, content := range evidence {
		rel, content := rel, content
		mountPath := filepath.Join(tp.AliceMountDir, filepath.FromSlash(rel))
		WaitForCondition(t, 30*time.Second, 200*time.Millisecond, func() bool {
			info, err := os.Stat(mountPath)
			return err == nil && info.Size() == int64(len(content))
		}, "waiting for "+rel+" on the analyst mount")
		got, err := os.ReadFile(mountPath)
		require.NoError(err, rel)
		require.Equal(content, got, "byte mismatch: %s", rel)
	}

	// B cannot contaminate the tree: local writes through the mount get EROFS.
	err := os.WriteFile(filepath.Join(tp.AliceMountDir, "dump", "note.txt"), []byte("x"), 0o644)
	require.Error(err, "analyst writes must be refused by the read-only mount")

	// B cannot push anything to A: the origin refuses every peer mutation.
	plant := filepath.Join(t.TempDir(), "plant.txt")
	require.NoError(os.WriteFile(plant, []byte("planted"), 0o644))
	require.Error(tp.Alice.AddFile(plant), "read-only origin must refuse the share")

	// B's saved bytes carry the origin metadata once complete.
	diskPath := filepath.Join(tp.AliceSaveDir, "dump", "memory.raw")
	WaitForCondition(t, 15*time.Second, 200*time.Millisecond, func() bool {
		info, err := os.Stat(diskPath)
		if err != nil {
			return false
		}
		d := info.ModTime().Sub(origMtime)
		if d < 0 {
			d = -d
		}
		return d <= time.Second
	}, "waiting for origin mtime on the analyst's saved bytes")
	info, err := os.Stat(diskPath)
	require.NoError(err)
	require.WithinDuration(origMtime, info.ModTime(), time.Second)
	if runtime.GOOS != "windows" {
		require.Equal(os.FileMode(0o640), info.Mode().Perm())
	}
	root := tp.Alice.FS.Root()
	root.RemoteFilesLock.RLock()
	f := root.RemoteFiles["/dump/memory.raw"]
	root.RemoteFilesLock.RUnlock()
	require.NotNil(f)
	require.Equal(origAtime.UnixNano(), f.OriginAtimeNs, "origin atime recorded on the analyst side")

	// A is untampered: same bytes, same mtime, same file set. Serving is
	// read-only (os.Open) and share_read_only blocked the plant upstream.
	seen := map[string]bool{}
	err = filepath.Walk(tp.BobSaveDir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || fi.Name() == ".DS_Store" {
			return err
		}
		rel, rerr := filepath.Rel(tp.BobSaveDir, p)
		if rerr != nil {
			return rerr
		}
		seen[filepath.ToSlash(rel)] = true
		return nil
	})
	require.NoError(err)
	require.Len(seen, len(evidence), "no files may appear or vanish on the evidence machine: %v", seen)
	for rel, content := range evidence {
		require.True(seen[rel], "evidence file missing on A: %s", rel)
		p := filepath.Join(tp.BobSaveDir, filepath.FromSlash(rel))
		got, rerr := os.ReadFile(p)
		require.NoError(rerr)
		require.Equal(content, got, "evidence bytes changed on A: %s", rel)
		sinfo, serr := os.Stat(p)
		require.NoError(serr)
		require.WithinDuration(origMtime, sinfo.ModTime(), time.Second, "evidence mtime changed on A: %s", rel)
	}
}
