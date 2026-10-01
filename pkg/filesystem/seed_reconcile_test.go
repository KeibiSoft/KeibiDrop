// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

// Chunk-cheap reconcile from local bytes (swap-save-stamps 2026-10-01, stage 3):
// when the pre-edit bitmap carries no fingerprints (an author's own file, a
// copy adopted from a sidecar) or the bytes moved to a conflict sibling, the
// reconcile hashes the local bytes and keeps every chunk equal to the peer's,
// copying it into the cache copy when it lives in another file. And the held
// version is recorded when bytes are served, not when the cache writer is
// done, so a read followed at once by a write starts its session at the
// version it read.

package filesystem

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
	"github.com/zeebo/xxh3"
)

func seedBytes(size int64) []byte {
	data := make([]byte, size)
	rand.New(rand.NewSource(20261001)).Read(data)
	return data
}

func chunkHashes(data []byte) map[int]uint64 {
	cs := ChunkSize
	out := make(map[int]uint64, len(data)/cs)
	for c := 0; (c+1)*cs <= len(data); c++ {
		out[c] = xxh3.Hash(data[c*cs : (c+1)*cs])
	}
	return out
}

// An author's own file superseded by a peer version: chunks whose bytes on
// disk hash to the peer's fingerprint are marked present without a fetch; a
// chunk the peer changed, and one the peer did not report, stay absent.
func TestReconcile_SeedsInPlaceFromLocalBytes(t *testing.T) {
	size := reconcileSize()
	data := seedBytes(size)
	real := filepath.Join(t.TempDir(), "own.bin")
	require.NoError(t, os.WriteFile(real, data, 0o644))
	peer := chunkHashes(data)
	peer[7] ^= 0xBAD
	delete(peer, 9)
	d := newReconcileDir(&hashingProvider{peerHashes: peer})
	f := &File{logger: nopLogger(), Root: d, RelativePath: "/own.bin", RealPathOfFile: real, stat: &winfuse.Stat_t{Size: size}}
	nb := NewChunkBitmap(size)
	f.Bitmap = nb

	d.reconcileEditAsync("/own.bin", f, nil, nb, size, size, real)

	require.Equal(t, reconcileChunks-2, nb.Have())
	require.False(t, nb.Has(7), "a chunk the peer changed must stay absent")
	require.False(t, nb.Has(9), "a chunk the peer did not report must stay absent")
	h, ok := nb.Hash(0)
	require.True(t, ok)
	require.Equal(t, peer[0], h)
	got, err := os.ReadFile(real)
	require.NoError(t, err)
	require.Equal(t, data, got, "seeding in place never writes the file")
}

// The bytes live in another file (the conflict sibling the loser's bytes
// moved to, or the canonical a sibling is seeded from): matching chunks are
// copied into the cache copy and marked; a differing chunk is left absent and
// its bytes untouched; a cache copy that does not exist yet is created at
// full size.
func TestReconcile_SeedsFromSibling(t *testing.T) {
	size := reconcileSize()
	data := seedBytes(size)
	dir := t.TempDir()
	seed := filepath.Join(dir, "doc.conflict-20261001-120000-1.bin")
	target := filepath.Join(dir, "doc.bin")
	require.NoError(t, os.WriteFile(seed, data, 0o644))
	peer := chunkHashes(data)
	peer[3] ^= 0xBAD
	d := newReconcileDir(&hashingProvider{peerHashes: peer})
	f := &File{logger: nopLogger(), Root: d, RelativePath: "/doc.bin", RealPathOfFile: target, stat: &winfuse.Stat_t{Size: size}}
	nb := NewChunkBitmap(size)
	f.Bitmap = nb

	d.reconcileEditAsync("/doc.bin", f, nil, nb, size, size, seed)

	require.Equal(t, reconcileChunks-1, nb.Have())
	require.False(t, nb.Has(3))
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Len(t, got, int(size), "the cache copy is created at full size")
	cs := ChunkSize
	for c := 0; c < reconcileChunks; c++ {
		if c == 3 {
			require.Equal(t, make([]byte, cs), got[c*cs:(c+1)*cs], "the differing chunk is never written")
			continue
		}
		require.Equal(t, data[c*cs:(c+1)*cs], got[c*cs:(c+1)*cs], "chunk %d", c)
	}
}

// With a pre-edit bitmap that has present bits but no fingerprints (loaded
// from a sidecar), only its present chunks are candidates: the rest of the
// file on disk is not the replaced version.
func TestReconcile_SeedHonoursPresentBits(t *testing.T) {
	size := reconcileSize()
	data := seedBytes(size)
	real := filepath.Join(t.TempDir(), "copy.bin")
	require.NoError(t, os.WriteFile(real, data, 0o644))
	old := NewChunkBitmap(size)
	old.Set(0)
	old.Set(5)
	require.False(t, old.HasHashes())
	d := newReconcileDir(&hashingProvider{peerHashes: chunkHashes(data)})
	f := &File{logger: nopLogger(), Root: d, RelativePath: "/copy.bin", RealPathOfFile: real, stat: &winfuse.Stat_t{Size: size}}
	nb := NewChunkBitmap(size)
	f.Bitmap = nb

	d.reconcileEditAsync("/copy.bin", f, old, nb, size, size, real)

	require.Equal(t, 2, nb.Have())
	require.True(t, nb.Has(0))
	require.True(t, nb.Has(5))
}

func TestConflictCanonical(t *testing.T) {
	cases := map[string]string{
		"/report.conflict-20260801-104512-123456789.docx": "/report.docx",
		"/a/b/notes.conflict-20260801-104512-1":           "/a/b/notes",
		"/x/a.tar.conflict-20260801-104512-5-2.gz":        "/x/a.tar.gz",
		"/plain.txt":               "",
		"/dir.conflict-x/file.txt": "",
	}
	for in, want := range cases {
		require.Equal(t, want, conflictCanonical(in), in)
	}
	rel, _ := conflictNames("/d/report.docx", filepath.Join(t.TempDir(), "report.docx"))
	require.Equal(t, "/d/report.docx", conflictCanonical(rel), "the inverse of conflictNames")
}

// A read of a never-fetched remote file followed at once by a write into the
// unit it fetched, the read handle still open: the session base must be the
// version the process read, although the cache writer that used to record
// the held version has not run yet (Linux, 2026-10-01: base -1, a conflict
// copy on the owner for a plain turn-taking patch).
func TestRead_HeldVersionAtServeTime(t *testing.T) {
	const size = int64(20 * 1048576)
	d, _, _, _ := newFillDir(t, size)
	d.RemoteFilesLock.RLock()
	rf := d.RemoteFiles["/big.bin"]
	d.RemoteFilesLock.RUnlock()
	require.NotNil(t, rf)

	rf.metaMu.Lock()
	remote := rf.RemoteMtimeNs
	rf.metaMu.Unlock()
	require.Positive(t, remote)

	rfi := &winfuse.FileInfo_t{Flags: os.O_RDONLY}
	require.Equal(t, 0, d.OpenEx("/big.bin", rfi))
	buf := make([]byte, 4096)
	require.Equal(t, 4096, d.Read("/big.bin", buf, 0, rfi.Fh))
	require.Equal(t, fillByte(0), buf[0])
	// Right after the read returns: the 16 MiB unit is still being written to
	// the cache by the async writer, which used to be the only place the held
	// version was recorded.
	rf.metaMu.Lock()
	heldNow := rf.HeldMtimeNs
	rf.metaMu.Unlock()
	require.Equal(t, remote, heldNow, "the held version is recorded when the bytes are served, not when the cache writer is done")

	wfi := &winfuse.FileInfo_t{Flags: os.O_RDWR}
	require.Equal(t, 0, d.OpenEx("/big.bin", wfi))
	// The patch lands in the unit the read fetched (no fill): the session
	// base is computed from what is held at this moment.
	require.Equal(t, 4, d.Write("/big.bin", []byte("EDIT"), 1048576, wfi.Fh))
	rf.metaMu.Lock()
	held, base := rf.HeldMtimeNs, rf.EditBaseMtimeNs
	rf.metaMu.Unlock()
	require.Equal(t, remote, held, "served bytes make the version held")
	require.Equal(t, remote, base, "the write session starts at the version the process read, not at -1")
	require.Equal(t, 0, d.Release("/big.bin", wfi.Fh))
	require.Equal(t, 0, d.Release("/big.bin", rfi.Fh))
}
