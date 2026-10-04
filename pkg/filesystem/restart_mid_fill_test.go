// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	winfuse "github.com/winfsp/cgofuse/fuse"
)

// stoppableProvider is a fillProvider whose fill can be refused: the link
// dropping before the fill starts, or the daemon stopping.
type stoppableProvider struct {
	fillProvider
	refuse atomic.Bool
	mu     sync.Mutex
	starts []string // one entry per StreamFile call: "<offset>" or "<offset>:refused"
}

func (p *stoppableProvider) StreamFile(ctx context.Context, path string, start uint64) (types.StreamFileReceiver, error) {
	refused := p.refuse.Load()
	if refused {
		p.streamCalls.Add(1) // the delegate counts the calls it serves
	}
	p.mu.Lock()
	entry := fmt.Sprint(start)
	if refused {
		entry += ":refused"
	}
	p.starts = append(p.starts, entry)
	p.mu.Unlock()
	if refused {
		return nil, io.EOF
	}
	return p.fillProvider.StreamFile(ctx, path, start)
}

func (p *stoppableProvider) calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.starts...)
}

// fillRunning reports whether any file of d still has a fill in flight.
func fillRunning(d *Dir) bool {
	busy := func(m map[string]*File) bool {
		for _, f := range m {
			if f.fillActive.Load() {
				return true
			}
		}
		return false
	}
	d.RemoteFilesLock.RLock()
	remote := busy(d.RemoteFiles)
	d.RemoteFilesLock.RUnlock()
	d.AfmLock.RLock()
	defer d.AfmLock.RUnlock()
	return remote || busy(d.AllFileMap)
}

// newDirOver is one daemon session over saveDir: a Dir wired to prov with
// its announces captured. Two of them in sequence over one saveDir are a
// restart.
func newDirOver(t *testing.T, saveDir string, prov types.FileStreamProvider) (*Dir, func() []types.FileEvent) {
	t.Helper()
	d := newTestDir(saveDir)
	ctx, cancel := context.WithCancel(context.Background())
	d.SetCtx(ctx)
	// A fill still landing bytes holds the cache copy open, and Windows cannot
	// remove the temp folder under an open file: the session ends first.
	t.Cleanup(func() {
		cancel()
		testkit.Eventually(t, 10*time.Second, 5*time.Millisecond, func() bool { return !fillRunning(d) }, "the session's fills to stop")
	})
	d.SetStreamProvider(func() types.FileStreamProvider { return prov })
	var events []types.FileEvent
	evMu := make(chan struct{}, 1)
	evMu <- struct{}{}
	d.SetOnLocalChange(func(ev types.FileEvent) {
		<-evMu
		events = append(events, ev)
		evMu <- struct{}{}
	})
	return d, func() []types.FileEvent {
		<-evMu
		out := append([]types.FileEvent(nil), events...)
		evMu <- struct{}{}
		return out
	}
}

const restartFillSize = int64(40 * 1048576)

// editThenStop is session one: the peer's 40 MiB file, a 1 MiB write at
// 1 MiB into the never-fetched copy, the link gone before the fill, close.
// Returns the version stamp, the payload and the sidecar path, with the dirty
// records on disk.
func editThenStop(t *testing.T, saveDir string, remote time.Time) (stamp int64, payload []byte, sidecar string) {
	t.Helper()
	prov := &stoppableProvider{fillProvider: fillProvider{size: restartFillSize}}
	d, snap := newDirOver(t, saveDir, prov)
	st := remoteStat(restartFillSize, remote)
	stamp = st.Mtim.Sec*1e9 + st.Mtim.Nsec
	require.NoError(t, d.AddRemoteFile(d.logger, "/big.bin", "big.bin", st))
	var gst winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/big.bin", &gst, 0))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDWR
	require.Equal(t, 0, d.OpenEx("/big.bin", fi))
	payload = bytes.Repeat([]byte{0xAB}, 1048576)
	require.Equal(t, len(payload), d.Write("/big.bin", payload, 1048576, fi.Fh))
	prov.refuse.Store(true)
	require.Equal(t, 0, d.Release("/big.bin", fi.Fh))
	require.Nil(t, lastAdd(snap(), "/big.bin"), "a copy with holes is not announced")

	sidecar = BitmapPath(filepath.Join(saveDir, "big.bin"))
	_, meta, err := LoadSidecar(sidecar, restartFillSize)
	require.NoError(t, err, "the sidecar is on disk when close returns")
	require.NotNil(t, meta)
	require.NotNil(t, meta.Ledger)
	require.True(t, meta.Ledger.AnyDirty(), "the records of the edit are on disk")
	require.Equal(t, stamp, meta.HeldStamp, "the sidecar names the version the bytes came from")
	require.Equal(t, stamp, meta.EditBase, "and the base of the edit")
	return stamp, payload, sidecar
}

func requireFilled(t *testing.T, saveDir string, payload []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(saveDir, "big.bin"))
	require.NoError(t, err)
	require.Equal(t, restartFillSize, int64(len(data)))
	require.Equal(t, payload, data[1048576:2*1048576], "the fill must not land the peer's bytes over the app's")
	for _, off := range []int64{0, 3 * 1048576, 20 * 1048576, restartFillSize - 1} {
		require.Equal(t, fillByte(off), data[off], "byte at %d must be the peer's", off)
	}
}

// A write into a cache copy, then the daemon stops before the fill. The
// next session restores the edit from the sidecar when the peer announces
// the version it started from, resumes the fill, and announces the edit
// with its base when the copy is complete. Nothing of the write is lost.
func TestRestartMidFill_EditSurvivesAndAnnounces(t *testing.T) {
	saveDir := t.TempDir()
	remote := time.Now().Add(-time.Minute)
	stamp, payload, sidecar := editThenStop(t, saveDir, remote)

	prov := &fillProvider{size: restartFillSize}
	d2, snap2 := newDirOver(t, saveDir, prov)
	require.NoError(t, d2.AddRemoteFile(d2.logger, "/big.bin", "big.bin", remoteStat(restartFillSize, remote)))
	require.Eventually(t, func() bool { return lastAdd(snap2(), "/big.bin") != nil },
		30*time.Second, 20*time.Millisecond, "the restored edit must be announced when its fill completes")
	add := lastAdd(snap2(), "/big.bin")
	require.Equal(t, restartFillSize, add.Attr.Size)
	require.Equal(t, stamp, add.BaseMtimeNs, "the announce carries the base the edit started from")
	require.Positive(t, prov.streamCalls.Load(), "the fill resumed through StreamFile")
	requireFilled(t, saveDir, payload)

	d2.AfmLock.RLock()
	f := d2.AllFileMap["/big.bin"]
	d2.AfmLock.RUnlock()
	require.NotNil(t, f)
	f.metaMu.RLock()
	require.True(t, f.Bitmap.IsComplete())
	require.False(t, f.AnnounceAfterFill)
	require.False(t, f.NotLocalSynced)
	require.False(t, f.Ledger.AnyDirty(), "announced: the peer judges the records from here")
	f.metaMu.RUnlock()
	_, err := os.Stat(sidecar)
	require.True(t, os.IsNotExist(err), "the sidecar goes with the complete copy")
}

// The same restart, but the app opens the file before the peer's announce,
// while the link is still down: the open adopts the copy with its edit, a
// read serves the written bytes, the fill the close starts fails, and the
// announce still resumes the fill and announces the edit.
func TestRestartMidFill_OpenBeforeAnnounce(t *testing.T) {
	saveDir := t.TempDir()
	remote := time.Now().Add(-time.Minute)
	stamp, payload, _ := editThenStop(t, saveDir, remote)

	prov := &stoppableProvider{fillProvider: fillProvider{size: restartFillSize}}
	prov.refuse.Store(true)
	d2, snap2 := newDirOver(t, saveDir, prov)
	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d2.OpenEx("/big.bin", fi))
	buf := make([]byte, 4096)
	require.Equal(t, 4096, d2.Read("/big.bin", buf, 1048576, fi.Fh))
	require.Equal(t, payload[:4096], buf, "the edit is served from the copy")
	require.Equal(t, 0, d2.Release("/big.bin", fi.Fh))
	require.Nil(t, lastAdd(snap2(), "/big.bin"), "nothing to announce while chunks are missing")
	require.Eventually(t, func() bool { return prov.streamCalls.Load() > 0 }, 10*time.Second, 10*time.Millisecond,
		"the close starts the fill, which the link refuses")
	d2.AfmLock.RLock()
	f2 := d2.AllFileMap["/big.bin"]
	d2.AfmLock.RUnlock()
	require.Eventually(t, func() bool { return !f2.fillActive.Load() }, 10*time.Second, 10*time.Millisecond,
		"the refused fill ends")

	prov.refuse.Store(false)
	require.NoError(t, d2.AddRemoteFile(d2.logger, "/big.bin", "big.bin", remoteStat(restartFillSize, remote)))
	require.Eventually(t, func() bool { return lastAdd(snap2(), "/big.bin") != nil },
		30*time.Second, 20*time.Millisecond, "the announce resumes the fill, which announces the edit")
	require.Equal(t, stamp, lastAdd(snap2(), "/big.bin").BaseMtimeNs)
	requireFilled(t, saveDir, payload)
}

// The peer changed the file while this daemon was down. The restored edit
// started from the old version, so the new one is a concurrent edit: the
// local version is preserved as a conflict copy and the canonical takes the
// peer's bytes. Neither version is lost.
func TestRestartMidFill_PeerEditedMeanwhile(t *testing.T) {
	saveDir := t.TempDir()
	remote := time.Now().Add(-time.Minute)
	stamp, payload, _ := editThenStop(t, saveDir, remote)

	prov := &fillProvider{size: restartFillSize}
	d2, snap2 := newDirOver(t, saveDir, prov)
	// The peer's edit came after this daemon's write (it was down since),
	// and started from the version the local edit started from.
	newer := remoteStat(restartFillSize, time.Now().Add(time.Second))
	require.NoError(t, d2.AddRemoteFileWithBase(d2.logger, "/big.bin", "big.bin", newer, stamp))

	matches, err := filepath.Glob(filepath.Join(saveDir, "big.conflict-*.bin"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "the local edit survives as a conflict copy")
	copyData, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	require.Equal(t, payload, copyData[1048576:2*1048576])
	require.NotNil(t, lastAdd(snap2(), filepath.ToSlash("/"+filepath.Base(matches[0]))), "the copy is announced")

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d2.OpenEx("/big.bin", fi))
	buf := make([]byte, 64)
	require.Equal(t, 64, d2.Read("/big.bin", buf, 1048576, fi.Fh))
	want := make([]byte, 64)
	for i := range want {
		want[i] = fillByte(1048576 + int64(i))
	}
	require.Equal(t, want, buf, "the canonical serves the peer's new version")
	require.Equal(t, 0, d2.Release("/big.bin", fi.Fh))
}

// A sidecar from one version does not vouch for another: the peer edited
// the file while this daemon was down, and the chunks the old session had
// landed are fetched again, never served as the new version.
func TestRestart_SidecarOfAnotherVersionIsNotTrusted(t *testing.T) {
	saveDir := t.TempDir()
	remote := time.Now().Add(-time.Minute)
	prov1 := &fillProvider{size: restartFillSize}
	d1, _ := newDirOver(t, saveDir, prov1)
	require.NoError(t, d1.AddRemoteFile(d1.logger, "/big.bin", "big.bin", remoteStat(restartFillSize, remote)))
	var gst winfuse.Stat_t
	require.Equal(t, 0, d1.Getattr("/big.bin", &gst, 0)) // the kernel looks the path up before the open
	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDONLY
	require.Equal(t, 0, d1.OpenEx("/big.bin", fi))
	buf := make([]byte, 4096)
	require.Equal(t, 4096, d1.Read("/big.bin", buf, 20*1048576, fi.Fh))
	require.Equal(t, 0, d1.Release("/big.bin", fi.Fh))
	bm, meta, err := LoadSidecar(BitmapPath(filepath.Join(saveDir, "big.bin")), restartFillSize)
	require.NoError(t, err)
	require.True(t, bm.HasRange(20*1048576, 4096), "session one left the chunk in the sidecar")
	require.NotNil(t, meta)
	require.Zero(t, meta.EditBase)

	prov2 := &fillProvider{size: restartFillSize}
	d2, _ := newDirOver(t, saveDir, prov2)
	require.NoError(t, d2.AddRemoteFile(d2.logger, "/big.bin", "big.bin", remoteStat(restartFillSize, remote.Add(30*time.Second))))
	require.Equal(t, 0, d2.Getattr("/big.bin", &gst, 0))
	require.Equal(t, 0, d2.OpenEx("/big.bin", fi))
	before := prov2.reads.Load()
	require.Equal(t, 4096, d2.Read("/big.bin", buf, 20*1048576, fi.Fh))
	require.Greater(t, prov2.reads.Load(), before, "the new version's bytes must come from the peer")
	require.Equal(t, 0, d2.Release("/big.bin", fi.Fh))
}

// The fill starts at close and resumes from the sidecar. A sidecar older
// than the chunks the session landed since must not replace the live
// bitmap: the fill would then fetch those chunks again and land the peer's
// bytes over the app's write.
func TestFillStart_KeepsTheLiveBitmap(t *testing.T) {
	d, saveDir, prov, snapshot := newFillDir(t, restartFillSize)
	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDWR
	require.Equal(t, 0, d.OpenEx("/big.bin", fi))
	buf := make([]byte, 4096)
	require.Equal(t, 4096, d.Read("/big.bin", buf, 20*1048576, fi.Fh))
	d.AfmLock.RLock()
	f := d.AllFileMap["/big.bin"]
	d.AfmLock.RUnlock()
	f.CacheWg.Wait()
	f.flushSidecar() // the sidecar knows the second unit only

	payload := bytes.Repeat([]byte{0xCD}, 1048576)
	require.Equal(t, len(payload), d.Write("/big.bin", payload, 1048576, fi.Fh))
	require.Equal(t, 0, d.Release("/big.bin", fi.Fh))
	require.Eventually(t, func() bool { return lastAdd(snapshot(), "/big.bin") != nil },
		30*time.Second, 20*time.Millisecond, "the fill announces on completion")
	require.Positive(t, prov.streamCalls.Load())
	data, err := os.ReadFile(filepath.Join(saveDir, "big.bin"))
	require.NoError(t, err)
	require.Equal(t, payload, data[1048576:2*1048576], "the fill must not land the peer's bytes over the app's write")
}

// The link drops when the fill starts. The fill ends short, and the next
// close of the file starts it again; the deferred announce goes out when
// that fill completes. A fill that could not finish must never keep the
// announce waiting for ever.
func TestFillRestartsAfterATransientFailure(t *testing.T) {
	saveDir := t.TempDir()
	prov := &stoppableProvider{fillProvider: fillProvider{size: restartFillSize}}
	d, snap := newDirOver(t, saveDir, prov)
	remote := time.Now().Add(-time.Minute)
	require.NoError(t, d.AddRemoteFile(d.logger, "/big.bin", "big.bin", remoteStat(restartFillSize, remote)))
	var gst winfuse.Stat_t
	require.Equal(t, 0, d.Getattr("/big.bin", &gst, 0))

	fi := &winfuse.FileInfo_t{}
	fi.Flags = os.O_RDWR
	require.Equal(t, 0, d.OpenEx("/big.bin", fi))
	payload := bytes.Repeat([]byte{0x5A}, 1048576)
	require.Equal(t, len(payload), d.Write("/big.bin", payload, 1048576, fi.Fh))
	prov.refuse.Store(true)
	require.Equal(t, 0, d.Release("/big.bin", fi.Fh))
	require.Eventually(t, func() bool { return prov.streamCalls.Load() == 1 }, 10*time.Second, 10*time.Millisecond)
	d.AfmLock.RLock()
	f := d.AllFileMap["/big.bin"]
	d.AfmLock.RUnlock()
	require.Eventually(t, func() bool { return !f.fillActive.Load() }, 10*time.Second, 10*time.Millisecond,
		"the failed fill must end")
	require.Nil(t, lastAdd(snap(), "/big.bin"))

	prov.refuse.Store(false)
	fi2 := &winfuse.FileInfo_t{}
	fi2.Flags = os.O_RDWR
	require.Equal(t, 0, d.OpenEx("/big.bin", fi2))
	require.Equal(t, 4, d.Write("/big.bin", []byte("more"), 2*1048576, fi2.Fh))
	require.Equal(t, 0, d.Release("/big.bin", fi2.Fh))
	require.Eventually(t, func() bool { return lastAdd(snap(), "/big.bin") != nil },
		30*time.Second, 20*time.Millisecond, "the second close must start the fill again and announce")
	require.Equal(t, int64(2), prov.streamCalls.Load(), "fills: %v", prov.calls())
	data, err := os.ReadFile(filepath.Join(saveDir, "big.bin"))
	require.NoError(t, err)
	require.Equal(t, payload, data[1048576:2*1048576])
	require.Equal(t, []byte("more"), data[2*1048576:2*1048576+4])
}
