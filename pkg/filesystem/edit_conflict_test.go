// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// seedLocalFile creates a local file through the FUSE surface so the object
// carries real authority state (LocalNewer, stat, session base).
func seedLocalFile(t *testing.T, d *Dir, rel, content string) *File {
	t.Helper()
	fi := &winfuse.FileInfo_t{}
	require.Equal(t, 0, d.CreateEx(rel, 0o644, fi))
	require.Equal(t, len(content), d.Write(rel, []byte(content), 0, fi.Fh))
	require.Equal(t, 0, d.Release(rel, fi.Fh))
	d.AfmLock.RLock()
	f := d.AllFileMap[rel]
	d.AfmLock.RUnlock()
	require.NotNil(t, f)
	return f
}

func remoteStat(size int64, mtime time.Time) *winfuse.Stat_t {
	st := &winfuse.Stat_t{Size: size, Mode: winfuse.S_IFREG | 0o644}
	st.Mtim = winfuse.NewTimespec(mtime)
	st.Ctim = st.Mtim
	st.Atim = st.Mtim
	return st
}

func newConflictTestDir(t *testing.T) (*Dir, func() []types.FileEvent) {
	t.Helper()
	d := newTestDir(t.TempDir())
	var mu sync.Mutex
	var events []types.FileEvent
	d.OnLocalChange = func(ev types.FileEvent) {
		mu.Lock()
		events = append(events, ev)
		mu.Unlock()
	}
	d.OpenStreamProvider = func() types.FileStreamProvider { return nil }
	snapshot := func() []types.FileEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]types.FileEvent(nil), events...)
	}
	return d, snapshot
}

func conflictSiblings(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".conflict-") {
			out = append(out, e.Name())
		}
	}
	return out
}

func localIdentity(f *File) int64 {
	f.metaMu.Lock()
	defer f.metaMu.Unlock()
	return f.stat.Mtim.Sec*1e9 + f.stat.Mtim.Nsec
}

// A peer EDIT whose declared base predates the local write is a provable
// concurrent edit: the local bytes must survive as a conflict sibling, the
// pending local announce must be cancelled, and the canonical cache must be
// resized for the winner (same contract as the ADD path).
func TestEditRemoteFileWithBase_StaleBasePreservesConflict(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "local-authoritative-content")
	identity := localIdentity(f)

	st := remoteStat(9, time.Now().Add(2*time.Second))
	require.NoError(t, d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, identity-1))

	conflicts := conflictSiblings(t, d.LocalDownloadFolder)
	require.Len(t, conflicts, 1, "exactly one conflict sibling expected")
	got, err := os.ReadFile(filepath.Join(d.LocalDownloadFolder, conflicts[0]))
	require.NoError(t, err)
	require.Equal(t, "local-authoritative-content", string(got))

	fi, err := os.Stat(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, err)
	require.EqualValues(t, 9, fi.Size(), "canonical cache must hold the winner's size")
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	f.metaMu.Unlock()
	require.False(t, localNewer, "authority hands over to the accepted winner")

	d.AfmLock.RLock()
	_, tracked := d.AllFileMap["/"+conflicts[0]]
	d.AfmLock.RUnlock()
	require.True(t, tracked, "conflict sibling must be registered")

	var sawCancel, sawConflictAdd bool
	for _, ev := range snapshot() {
		if ev.Action == types.CancelPendingNotify && ev.Path == "/doc.txt" {
			sawCancel = true
		}
		if ev.Action == types.AddFile && strings.Contains(ev.Path, ".conflict-") {
			sawConflictAdd = true
		}
	}
	require.True(t, sawCancel, "acceptance must cancel the pending local announce")
	require.True(t, sawConflictAdd, "conflict sibling must be announced to the peer")
}

// Turn-taking: a base equal to the local identity proves the editor saw our
// version. Plain acceptance, no copy.
func TestEditRemoteFileWithBase_CurrentBaseNoConflict(t *testing.T) {
	d, _ := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "local-authoritative-content")
	identity := localIdentity(f)

	st := remoteStat(9, time.Now().Add(2*time.Second))
	require.NoError(t, d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, identity))

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
}

// Base 0 (unknown, pre-T3b peer): plain LWW, never a conflict copy.
func TestEditRemoteFileWithBase_UnknownBaseNoConflict(t *testing.T) {
	d, _ := newConflictTestDir(t)
	seedLocalFile(t, d, "/doc.txt", "local-authoritative-content")

	st := remoteStat(9, time.Now().Add(2*time.Second))
	require.NoError(t, d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, 0))

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
}

// A stale EDIT (older than the local write) keeps local authority: rejected
// with no preserve, no announce cancellation side effects on the bytes.
func TestEditRemoteFileWithBase_StaleEditRejected(t *testing.T) {
	d, _ := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "local-authoritative-content")

	st := remoteStat(9, time.Now().Add(-2*time.Second))
	err := d.EditRemoteFileWithBase(nopLogger(), "/doc.txt", "doc.txt", st, 1)
	require.Error(t, err)

	require.Empty(t, conflictSiblings(t, d.LocalDownloadFolder))
	got, rerr := os.ReadFile(filepath.Join(d.LocalDownloadFolder, "doc.txt"))
	require.NoError(t, rerr)
	require.Equal(t, "local-authoritative-content", string(got))
	f.metaMu.Lock()
	localNewer := f.LocalNewer
	f.metaMu.Unlock()
	require.True(t, localNewer, "rejected edit must not take authority")
}
