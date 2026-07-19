// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// Characterizes EAGER metadata at scale. oak.space reached ~100k files using LAZY
// metadata; KeibiDrop announces every file up front (ADD_FILE per file), so this test
// measures the cost of that — propagation time + receiver heap — and REPORTS it. Opt-in
// via KD_SCALE_N (e.g. 100000) so it never slows the normal suite.

package tests

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// bobRemoteCount returns how many files the receiver currently tracks (the eager metadata
// set), read under the same lock the harness uses.
func bobRemoteCount(tp *TestPair) int {
	tp.Bob.SyncTracker.RemoteFilesMu.RLock()
	defer tp.Bob.SyncTracker.RemoteFilesMu.RUnlock()
	return len(tp.Bob.SyncTracker.RemoteFiles)
}

// TestScale_ManyFiles: Alice announces N tiny files (every one a separate ADD_FILE the
// peer must track); we measure how fast the peer receives the full set and its heap, then
// byte-verify a spread-out sample. Run: KD_SCALE_N=100000 go test -run TestScale_ManyFiles -v
func TestScale_ManyFiles(t *testing.T) {
	v := os.Getenv("KD_SCALE_N")
	if v == "" {
		t.Skip("set KD_SCALE_N to run the eager-metadata scale characterization (e.g. 100000)")
	}
	N, err := strconv.Atoi(v)
	if err != nil || N <= 0 {
		t.Fatalf("KD_SCALE_N=%q is not a positive integer", v)
	}

	tp := SetupPeerPairWithTimeout(t, false, 600*time.Second) // no-FUSE: pure ADD_FILE metadata path

	content := []byte("x") // tiny: we measure metadata scale, not transfer throughput

	// Create the files first. This is a TEST artifact (a real share's files already exist
	// on disk), timed separately so it doesn't pollute the eager-metadata cost.
	createStart := time.Now()
	paths := make([]string, N)
	for i := 0; i < N; i++ {
		paths[i] = filepath.Join(tp.AliceSaveDir, fmt.Sprintf("f%06d", i))
		if werr := os.WriteFile(paths[i], content, 0644); werr != nil {
			t.Fatalf("write %d: %v", i, werr)
		}
	}
	createElapsed := time.Since(createStart)

	// THIS is the eager-metadata cost: AddFile indexes each file and queues an ADD_FILE
	// the peer must receive. (Templating would shrink the bytes of that ADD_FILE, not this.)
	addStart := time.Now()
	for i := 0; i < N; i++ {
		if aerr := tp.Alice.AddFile(paths[i]); aerr != nil {
			t.Fatalf("AddFile %d: %v", i, aerr)
		}
	}
	addElapsed := time.Since(addStart)

	// Wait for the eager metadata to fully land on the peer.
	syncStart := time.Now()
	deadline := time.Now().Add(540 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		if got = bobRemoteCount(tp); got >= N {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	syncElapsed := time.Since(syncStart)

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	// Byte-verify a spread-out sample (on-demand pull).
	sample := []int{0, 1, N / 4, N / 2, 3 * N / 4, N - 2, N - 1}
	okN := 0
	for _, i := range sample {
		name := fmt.Sprintf("f%06d", i)
		dest := filepath.Join(tp.BobSaveDir, name)
		if perr := tp.Bob.PullFile(name, dest); perr != nil {
			continue
		}
		if b, rerr := os.ReadFile(dest); rerr == nil && bytes.Equal(b, content) {
			okN++
		}
	}

	t.Logf("SCALE N=%d received=%d create=%s share=%s sync=%s shareRate=%.0f/s heapAllocMiB=%d sample_ok=%d/%d",
		N, got, createElapsed.Round(time.Millisecond), addElapsed.Round(time.Millisecond),
		syncElapsed.Round(time.Millisecond), float64(N)/addElapsed.Seconds(),
		ms.HeapAlloc/(1024*1024), okN, len(sample))

	if okN != len(sample) {
		t.Errorf("sample correctness: %d/%d sample files read back wrong", okN, len(sample))
	}
	if got < N {
		t.Errorf("eager metadata incomplete: peer received %d/%d within %s (the headline result)",
			got, N, syncElapsed.Round(time.Millisecond))
	}
}
