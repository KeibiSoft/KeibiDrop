// ABOUTME: No-FUSE PullFile throughput benchmark for regression detection.
// ABOUTME: Measures direct PullFile() speed at 10MB, 100MB, and 1GB sizes.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestPullFileThroughput measures PullFile() throughput without FUSE overhead.
// Alice shares files; Bob pulls them directly. Skipped in short mode.
func TestPullFileThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping throughput benchmark in short mode")
	}

	tp := SetupPeerPairWithTimeout(t, false, 600*time.Second)

	sizes := []struct {
		name string
		size int
	}{
		{"10MB", 10 * 1024 * 1024},
		{"100MB", 100 * 1024 * 1024},
		{"1GB", 1024 * 1024 * 1024},
	}

	t.Logf("=== no-FUSE PullFile Throughput ===")

	for _, s := range sizes {
		s := s // capture loop variable
		t.Run(s.name, func(t *testing.T) {
			req := require.New(t)

			fileName := fmt.Sprintf("bench_pull_%s.bin", s.name)

			// Generate random payload and write to Alice's save dir
			data := make([]byte, s.size)
			_, err := rand.Read(data)
			req.NoError(err)

			alicePath := filepath.Join(tp.AliceSaveDir, fileName)
			req.NoError(os.WriteFile(alicePath, data, 0644))
			req.NoError(tp.Alice.AddFile(alicePath))

			// Wait for Bob's SyncTracker to see the file from Alice
			WaitForRemoteFile(t, tp.Bob.SyncTracker, fileName, 30*time.Second)

			// Drop page cache — ignore error if not running as root
			exec.Command("sudo", "sh", "-c",
				"sync && echo 3 > /proc/sys/vm/drop_caches").Run() //nolint:errcheck

			// Timed pull
			destPath := filepath.Join(tp.BobSaveDir, "pulled_"+fileName)
			start := time.Now()
			err = tp.Bob.PullFile(fileName, destPath)
			elapsed := time.Since(start)
			req.NoError(err)

			// Verify the pulled file has the correct size
			info, err := os.Stat(destPath)
			req.NoError(err)
			req.Equal(int64(s.size), info.Size(), "pulled file size mismatch")

			mbps := float64(s.size) / elapsed.Seconds() / (1024 * 1024)
			fmt.Printf("BENCH\t%d\t%.2f\t%.3f\n", s.size, mbps, elapsed.Seconds())
		})
	}
}
