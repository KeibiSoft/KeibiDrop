// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

//go:build !windows

// DFIR remote-triage workload over the mount: an analyst machine (Alice,
// FUSE) triages a compromised-host tree that lives on the victim machine
// (Bob) without pulling the whole tree first. Walk, signature pass over
// the head of every file, IOC grep across text artifacts, then a
// selective KAPE-style extract with hash verification. This is the
// cold-on-demand sparse-read path under a many-files tree, the same shape
// the git cold-read work chases, with byte-exactness asserted at the end.
// Uses cp -R inside the share, so Unix-only; the Windows pass is the
// manual device run.

package tests

import (
	"bytes"
	"crypto/sha256"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/tests/dfirgen"
	"github.com/stretchr/testify/require"
)

func TestFUSEDFIRTriage(t *testing.T) {
	p := connectFUSEPeers(t, false)
	req := require.New(t)

	// Deterministic synthetic triage tree, staged outside the share. The
	// staging copy is the ground truth for every hash check below.
	staging := filepath.Join(t.TempDir(), "host1")
	sum, err := dfirgen.Generate(staging, 48, 1)
	req.NoError(err)
	t.Logf("generated %d files, %d MB, %d IOC files, %d extract artifacts",
		sum.Files, sum.Bytes>>20, sum.IOCFiles, len(sum.Extract))

	// Ship the tree into Bob's share the same way the git tests do: one
	// exec inside the save dir, so the watcher sees it like real writes.
	resp := p.bob.send(t, "exec . cp -R "+staging+" host1", 180*time.Second)
	req.Truef(strings.HasPrefix(resp, "EXEC:0:"), "cp -R into share: %s", resp)
	p.bob.send(t, "write_file host1/COMPLETE done", 10*time.Second)

	mountTree := filepath.Join(p.aliceMount, "host1")
	req.Eventually(func() bool {
		_, err := os.Stat(filepath.Join(mountTree, "COMPLETE"))
		return err == nil
	}, 180*time.Second, 500*time.Millisecond, "tree never finished syncing")

	// Phase 1: walk. The ReadDir and Lookup storm of a triage tool.
	start := time.Now()
	var files []string
	req.NoError(filepath.WalkDir(mountTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() != "COMPLETE" {
			files = append(files, path)
		}
		return nil
	}))
	t.Logf("walk: %d files in %v", len(files), time.Since(start))
	req.Equal(sum.Files, len(files), "mount must show exactly the generated tree")

	// Phase 2: signature pass. First bytes of EVERY file, cold, on demand.
	start = time.Now()
	head := make([]byte, 8)
	for _, f := range files {
		fh, err := os.Open(f) // #nosec G304 -- walking our own mount
		req.NoError(err)
		_, err = io.ReadFull(fh, head)
		req.NoError(fh.Close())
		req.NoErrorf(err, "head read %s", f)
	}
	t.Logf("signature pass: %d heads in %v", len(files), time.Since(start))

	// Phase 3: IOC grep across the text artifacts only.
	start = time.Now()
	hits := 0
	for _, f := range files {
		if !strings.HasSuffix(f, ".log") && !strings.HasSuffix(f, ".txt") {
			continue
		}
		data, err := os.ReadFile(f) // #nosec G304 -- walking our own mount
		req.NoError(err)
		if bytes.Contains(data, []byte(dfirgen.IOCMarker)) {
			hits++
		}
	}
	t.Logf("ioc grep: %d hits in %v", hits, time.Since(start))
	req.Equal(sum.IOCFiles, hits, "grep over the mount must find every planted marker")

	// Phase 4: selective extract of the KAPE-style set, hash-verified
	// against the staging truth. Chain of custody is the product claim.
	start = time.Now()
	outDir := t.TempDir()
	var extracted int64
	for _, rel := range sum.Extract {
		data, err := os.ReadFile(filepath.Join(mountTree, rel)) // #nosec G304
		req.NoErrorf(err, "extract %s", rel)
		orig, err := os.ReadFile(filepath.Join(staging, rel)) // #nosec G304
		req.NoError(err)
		req.Equalf(sha256.Sum256(orig), sha256.Sum256(data), "hash mismatch on %s", rel)
		dst := filepath.Join(outDir, rel)
		req.NoError(os.MkdirAll(filepath.Dir(dst), 0755))
		req.NoError(os.WriteFile(dst, data, 0644))
		extracted += int64(len(data))
	}
	t.Logf("extract+verify: %d artifacts, %d MB in %v",
		len(sum.Extract), extracted>>20, time.Since(start))
}
