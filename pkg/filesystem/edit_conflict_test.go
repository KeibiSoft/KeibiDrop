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

// Release must not announce a time below the in-memory identity. Write
// sets f.stat.Mtim from the Go clock. The disk mtime comes from the
// coarse kernel tick and can be older. An announce below the identity
// makes two concurrent writers reject each other and never converge.
func TestRelease_AnnounceCarriesInMemoryIdentity(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	fi := &winfuse.FileInfo_t{}
	require.Equal(t, 0, d.CreateEx("/doc.txt", 0o644, fi))
	require.Equal(t, 7, d.Write("/doc.txt", []byte("payload"), 0, fi.Fh))

	d.AfmLock.RLock()
	f := d.AllFileMap["/doc.txt"]
	d.AfmLock.RUnlock()
	require.NotNil(t, f)

	// Force the disk-coarse vs Go-clock split deterministically.
	f.metaMu.Lock()
	f.stat.Mtim = winfuse.NewTimespec(time.Now().Add(50 * time.Millisecond))
	memNs := f.stat.Mtim.Sec*1e9 + f.stat.Mtim.Nsec
	f.metaMu.Unlock()

	require.Equal(t, 0, d.Release("/doc.txt", fi.Fh))

	var addMtime uint64
	for _, ev := range snapshot() {
		if ev.Action == types.AddFile && ev.Path == "/doc.txt" {
			addMtime = ev.Attr.ModificationTime
		}
	}
	require.NotZero(t, addMtime, "Release must announce the edited file")
	require.GreaterOrEqual(t, addMtime, uint64(memNs),
		"announce must not understate the in-memory identity")
	f.metaMu.Lock()
	lastAnnounced := f.LastAnnouncedMtimeNs
	f.metaMu.Unlock()
	require.EqualValues(t, addMtime, lastAnnounced,
		"watermark must equal the announced stamp")
}

// A blind overwrite must announce the version it HELD, not the version it
// merely accepted. Accept a newer announce with no fetch, then overwrite:
// the base must stay at the held version, so the peer's predicate preserves
// the bytes this writer never saw (model v3). After a fetch lands, the same
// overwrite is turn-taking: base equals the accepted version.
func TestBlindOverwrite_BaseIsHeldVersion(t *testing.T) {
	d, snapshot := newConflictTestDir(t)
	f := seedLocalFile(t, d, "/doc.txt", "held-content")
	held := localIdentity(f)

	// Metadata-only accept of a newer peer version: no bytes fetched. Set
	// the fields acceptance sets (watermark up, authority handed over); the
	// full disk flow needs a stream provider this unit dir does not have.
	remote := time.Now().Add(2 * time.Second)
	f.metaMu.Lock()
	f.RemoteMtimeNs = remote.UnixNano()
	f.LocalNewer = false
	f.metaMu.Unlock()

	lastBase := func() int64 {
		base := int64(-99)
		for _, ev := range snapshot() {
			if ev.Action == types.AddFile && ev.Path == "/doc.txt" {
				base = ev.BaseMtimeNs
			}
		}
		return base
	}

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_WRONLY | os.O_TRUNC
	require.Equal(t, 0, d.OpenEx("/doc.txt", fi))
	require.Equal(t, 5, d.Write("/doc.txt", []byte("blind"), 0, fi.Fh))
	require.Equal(t, 0, d.Release("/doc.txt", fi.Fh))
	require.Equal(t, held, lastBase(),
		"blind overwrite must announce the held version as its base")

	// A landed fetch raises the held stamp: the next overwrite is honest
	// turn-taking with the accepted version as base.
	f.finishBlockFetch(0, &blockFetch{done: make(chan struct{})}, nil)
	fi2 := &winfuse.FileInfo_t{}
	fi2.Flags = os.O_WRONLY | os.O_TRUNC
	require.Equal(t, 0, d.OpenEx("/doc.txt", fi2))
	require.Equal(t, 7, d.Write("/doc.txt", []byte("fetched"), 0, fi2.Fh))
	require.Equal(t, 0, d.Release("/doc.txt", fi2.Fh))
	require.Equal(t, remote.UnixNano(), lastBase(),
		"post-fetch overwrite must announce the accepted version as its base")
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
