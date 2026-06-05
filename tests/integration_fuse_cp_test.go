// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

//go:build !windows

// This cp/dd throughput test uses syscall.Stat_t (st_blksize) and the cp/dd
// CLIs, so it is Unix-only (macOS + Linux). The Windows equivalent would use
// Copy-Item and is left for the Windows device run.

package tests

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// blksizeOf returns the st_blksize the kernel reports for path (what cp/dd use
// to size their I/O buffer). This is the value the FUSE Getattr returns.
func blksizeOf(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	require.Truef(t, ok, "Sys() not *syscall.Stat_t")
	return int64(st.Blksize)
}

// shareRemote makes Bob share a fresh size-byte random file named `name`, waits
// until it appears in Alice's mount, and returns the path inside Alice's mount.
func shareRemote(t *testing.T, p *fusePeerPair, name string, size int) string {
	t.Helper()
	data := make([]byte, size)
	_, err := rand.Read(data)
	require.NoError(t, err)
	bobPath := filepath.Join(p.bobSave, name)
	require.NoError(t, os.WriteFile(bobPath, data, 0644))
	require.Equal(t, "OK", p.bob.send(t, "add "+bobPath, 15*time.Second))
	require.Equal(t, "OK", p.alice.send(t, "wait_file "+name+" 30", 35*time.Second))
	return filepath.Join(p.aliceMount, name)
}

// TestFUSELocalCp1GB isolates the pure FUSE layer (NO peer streaming): cp a 1 GB
// file from outside the mount INTO it (write), then from inside the mount back
// OUT (read of a file that is LOCAL to this peer, served by the local fast path).
// This separates FUSE+blksize overhead from the peer-to-peer stream/fetch path.
func TestFUSELocalCp1GB(t *testing.T) {
	p := connectFUSEPeers(t, false)
	req := require.New(t)
	const sz = 1024 * 1024 * 1024 // 1 GiB

	// urandom source (NOT zeros) so cp can't sparse-optimize and report bogus speed.
	src := filepath.Join(t.TempDir(), "src1gb.bin")
	ddOut, ddErr := exec.Command("dd", "if=/dev/urandom", "of="+src, "bs=1M", "count=1024").CombinedOutput()
	req.NoErrorf(ddErr, "dd create src: %s", ddOut)

	// cp from OUTSIDE the mount INTO the mount (write path).
	inDst := filepath.Join(p.aliceMount, "local1gb.bin")
	start := time.Now()
	out, err := exec.Command("cp", src, inDst).CombinedOutput()
	inDur := time.Since(start)
	req.NoErrorf(err, "cp into mount: %s", out)
	fi, statErr := os.Stat(inDst)
	req.NoError(statErr)
	req.Equal(int64(sz), fi.Size(), "cp into mount wrong size")
	t.Logf("LOCAL cp 1GB  OUTSIDE -> INSIDE mount (write): %6.1f MB/s  (%.2fs)",
		float64(sz)/1e6/inDur.Seconds(), inDur.Seconds())

	// cp from INSIDE the mount back OUTSIDE (read of a local file — no streaming).
	outDst := filepath.Join(t.TempDir(), "out1gb.bin")
	start = time.Now()
	out, err = exec.Command("cp", inDst, outDst).CombinedOutput()
	outDur := time.Since(start)
	req.NoErrorf(err, "cp out of mount: %s", out)
	fi, statErr = os.Stat(outDst)
	req.NoError(statErr)
	req.Equal(int64(sz), fi.Size(), "cp out of mount wrong size")
	t.Logf("LOCAL cp 1GB  INSIDE -> OUTSIDE mount (read):  %6.1f MB/s  (%.2fs)",
		float64(sz)/1e6/outDur.Seconds(), outDur.Seconds())
}

// TestFUSECpThroughput measures REAL `cp`/`dd` throughput against the FUSE mount
// — the user-visible "copy a file to/from the mount" case. Unlike the in-test
// benchmarks (fixed Go read buffer), cp/dd size their buffer from st_blksize, so
// this is the test that proves the Linux st_blksize fix (report our 256 KiB
// FilesystemBlockSize, not ext4's 4 KiB) actually makes transfers fast.
func TestFUSECpThroughput(t *testing.T) {
	p := connectFUSEPeers(t, false) // on-demand (prefetch off): real peer streaming
	req := require.New(t)

	// Headline: cp a large remote file FROM the mount (peer -> FUSE on-demand read).
	const big = 128 * 1024 * 1024
	mountFile := shareRemote(t, p, "read_big.bin", big)
	bs := blksizeOf(t, mountFile)
	t.Logf("st_blksize reported by mount = %d bytes (expect our FilesystemBlockSize, not 4096)", bs)

	readOut := filepath.Join(t.TempDir(), "read_out.bin")
	start := time.Now()
	out, err := exec.Command("cp", mountFile, readOut).CombinedOutput()
	dur := time.Since(start)
	req.NoErrorf(err, "cp from mount failed: %s", out)
	fi, statErr := os.Stat(readOut)
	req.NoError(statErr)
	req.Equal(int64(big), fi.Size(), "cp produced wrong size")
	t.Logf("cp FROM mount (read, peer->FUSE on-demand): %6.1f MB/s  (%.2fs, %d MB)",
		float64(big)/1e6/dur.Seconds(), dur.Seconds(), big/(1024*1024))

	// cp a large file INTO the mount (write path the user described).
	writeSrc := filepath.Join(t.TempDir(), "write_src.bin")
	srcData := make([]byte, big)
	_, _ = rand.Read(srcData)
	req.NoError(os.WriteFile(writeSrc, srcData, 0644))
	writeDst := filepath.Join(p.aliceMount, "write_big.bin")
	start = time.Now()
	out, err = exec.Command("cp", writeSrc, writeDst).CombinedOutput()
	dur = time.Since(start)
	req.NoErrorf(err, "cp into mount failed: %s", out)
	t.Logf("cp INTO mount (write):                       %6.1f MB/s  (%.2fs, %d MB)",
		float64(big)/1e6/dur.Seconds(), dur.Seconds(), big/(1024*1024))

	// Block-size sensitivity: dd a fresh (uncached) remote file at each bs. Small
	// bs = a FUSE round trip per tiny read; this is what crawls on Linux when
	// st_blksize is tiny. Each file is read once (first read = real streaming).
	const small = 16 * 1024 * 1024
	for _, blk := range []string{"4k", "64k", "256k", "1M"} {
		f := shareRemote(t, p, fmt.Sprintf("dd_%s.bin", blk), small)
		start = time.Now()
		out, err = exec.Command("dd", "if="+f, "of=/dev/null", "bs="+blk).CombinedOutput()
		dur = time.Since(start)
		req.NoErrorf(err, "dd bs=%s failed: %s", blk, out)
		t.Logf("dd bs=%-4s FROM mount:                        %6.1f MB/s  (%.2fs)",
			blk, float64(small)/1e6/dur.Seconds(), dur.Seconds())
	}
}
