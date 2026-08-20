// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Announce-time benchmark for a scanned nested tree, the demo shape
// ABOUTME: that took ~100 s for 2,600 files. Convicts per-item receiver cost.

package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// TestAnnounceNestedTree measures the wall time from connect until every file
// of a scanned nested tree is announced into the FUSE receiver's remote set.
// The field measurement was ~38 ms per item; loopback should be orders of
// magnitude below that. The assert is a loose ceiling that still catches a
// per-item round trip or per-item disk-sync disease.
func TestAnnounceNestedTree(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping announce benchmark in short mode")
	}
	skipIfNoFUSE(t)
	require := require.New(t)

	count := 2600
	if v, err := strconv.Atoi(os.Getenv("KD_ANNOUNCE_COUNT")); err == nil && v >= 100 {
		count = v
	}

	// Nested shape: files spread over dirs three levels deep, like a triage
	// tree, so directory auto-creation is exercised too.
	var rels []string
	for i := range count {
		rels = append(rels, fmt.Sprintf("host1/sub_%02d/leaf_%d/f_%04d.bin", i%20, i%5, i))
	}

	start := time.Now()
	tp := SetupFUSEPeerPairPreConnect(t, 600*time.Second,
		func(alice, bob *common.KeibiDrop, aliceSave, bobSave string) {
			for _, rel := range rels {
				p := filepath.Join(bobSave, filepath.FromSlash(rel))
				require.NoError(os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(os.WriteFile(p, []byte("x"), 0o644))
			}
			bob.ScanSharedOnStart = true
		})
	setupDone := time.Now()
	waitForFUSEMount(t, tp.AliceMountDir, 15*time.Second)

	root := tp.Alice.FS.Root()
	require.NotNil(root)
	WaitForCondition(t, 300*time.Second, 100*time.Millisecond, func() bool {
		root.RemoteFilesLock.RLock()
		defer root.RemoteFilesLock.RUnlock()
		return len(root.RemoteFiles) >= count
	}, "waiting for the full announce")
	announceDone := time.Now()

	announceWall := announceDone.Sub(setupDone)
	perItem := announceWall / time.Duration(count)
	t.Logf("BENCH announce_nested files=%d setup=%s announce_wall=%s per_item=%s",
		count, setupDone.Sub(start), announceWall, perItem)

	// Loopback ceiling: 5 ms per item is already 100x a map insert and far
	// below the 38 ms per item measured over the WAN. Above it, something on
	// the announce path costs per-item time it should not.
	require.Less(perItem, 5*time.Millisecond,
		"announce cost %s per item on loopback: per-item disease on the announce path", perItem)
}
