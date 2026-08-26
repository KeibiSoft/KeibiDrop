// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Exact version-stamp ties (observed once in CI). The rank rule makes exactly
// one side accept; the loser preserves. Model twin: twopeer_tie_model_test.go.

package filesystem

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	winfuse "github.com/winfsp/cgofuse/fuse"

	"github.com/stretchr/testify/require"
)

// setExactIdentity pins the identity so the incoming announce ties it.
func setExactIdentity(f *File, ts time.Time) {
	f.metaMu.Lock()
	f.stat.Mtim = winfuse.NewTimespec(ts)
	f.metaMu.Unlock()
}

// The winner: a tied ADD stays rejected, local authority survives.
func TestTieBreak_AddWinnerKeepsRejecting(t *testing.T) {
	d, _ := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "winner-side-content")
	tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	setExactIdentity(f, tie)

	st := remoteStat(10, tie)
	_ = d.AddRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, tie.UnixNano()-5e9)

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
	got, err := os.ReadFile(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, "winner-side-content", string(got))
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	f.metaMu.Unlock()
	require.True(t, localNewer, "a tied ADD without rank must keep local authority")
}

// The loser: the tied ADD is accepted and the concurrent local bytes
// survive as a conflict sibling.
func TestTieBreak_AddLoserAcceptsAndPreserves(t *testing.T) {
	d, _ := newConflictTestDir(t)
	d.Root.TieBreakPeerWins.Store(true)
	f := seedLocalFile(t, d, "/doc.txt", "loser-side-content")
	tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	setExactIdentity(f, tie)

	st := remoteStat(10, tie)
	require.NoError(t, d.AddRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, tie.UnixNano()-5e9))

	conflicts := conflictSiblings(t, d.LocalDownloadFolder)
	require.Len(t, conflicts, 1, "the tie loser must preserve its bytes")
	got, err := os.ReadFile(filepath.Join(d.LocalDownloadFolder, conflicts[0]))
	require.NoError(t, err)
	require.Equal(t, "loser-side-content", string(got))
	fi, err := os.Stat(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, err)
	require.EqualValues(t, 10, fi.Size(), "canonical cache must hold the winner's size")
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	f.metaMu.Unlock()
	require.False(t, localNewer, "the tie loser hands authority to the winner")
}

// A tie with the remote watermark is a redelivery: no acceptance, no
// sibling. Same size on purpose: a size change rides the refresh path.
func TestTieBreak_AddRedeliveryMintsNoConflict(t *testing.T) {
	d, _ := newConflictTestDir(t)
	d.Root.TieBreakPeerWins.Store(true)
	f := seedLocalFile(t, d, "/doc.txt", "settled-content")
	tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	f.metaMu.Lock()
	f.RemoteMtimeNs = tie.UnixNano()
	f.LocalNewer = false
	f.metaMu.Unlock()

	st := remoteStat(int64(len("settled-content")), tie)
	_ = d.AddRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, 0)

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
	got, err := os.ReadFile(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, err)
	require.Equal(t, "settled-content", string(got), "a redelivered tie must not clobber the cache")
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	f.metaMu.Unlock()
	require.False(t, localNewer, "redelivery must not fabricate authority")
}

// EDIT winner side: before the rank an equal stamp passed and tied dirty
// peers swapped contents. The winner must reject.
func TestTieBreak_EditWinnerRejectsTie(t *testing.T) {
	d, _ := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "winner-edit-content")
	tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	setExactIdentity(f, tie)

	st := remoteStat(10, tie)
	err := d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, tie.UnixNano()-5e9)
	require.Error(t, err, "a tied EDIT without rank must be rejected, not adopted")

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
	got, rerr := os.ReadFile(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, rerr)
	require.Equal(t, "winner-edit-content", string(got))
}

// EDIT loser side: accepted, local bytes survive as a sibling.
func TestTieBreak_EditLoserAcceptsAndPreserves(t *testing.T) {
	d, _ := newConflictTestDir(t)
	d.Root.TieBreakPeerWins.Store(true)
	f := seedLocalFile(t, d, "/doc.txt", "loser-edit-content")
	tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
	setExactIdentity(f, tie)

	st := remoteStat(10, tie)
	require.NoError(t, d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, tie.UnixNano()-5e9))

	conflicts := conflictSiblings(t, d.LocalDownloadFolder)
	require.Len(t, conflicts, 1, "the tie loser must preserve its bytes")
	got, err := os.ReadFile(filepath.Join(d.LocalDownloadFolder, conflicts[0]))
	require.NoError(t, err)
	require.Equal(t, "loser-edit-content", string(got))
}

// An equal stamp when clean is a metadata refresh: accepted on both ranks.
func TestTieBreak_EditEqualNotDirtyStillAccepted(t *testing.T) {
	for _, peerWins := range []bool{false, true} {
		d, _ := newConflictTestDir(t)
		d.Root.TieBreakPeerWins.Store(peerWins)
		f := seedLocalFile(t, d, "/doc.txt", "settled-content")
		tie := time.Now().Add(2 * time.Second).Truncate(time.Microsecond)
		f.metaMu.Lock()
		f.stat.Mtim = winfuse.NewTimespec(tie)
		f.RemoteMtimeNs = tie.UnixNano()
		f.LocalNewer = false
		f.metaMu.Unlock()

		st := remoteStat(int64(len("settled-content")), tie)
		require.NoError(t, d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, tie.UnixNano()),
			"peerWins=%v: an equal-stamp metadata refresh must stay accepted", peerWins)
		require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
	}
}
