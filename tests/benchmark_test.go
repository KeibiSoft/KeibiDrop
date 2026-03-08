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
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkLocalDisk provides baseline for comparison
func BenchmarkLocalDisk(b *testing.B) {
	tmpDir, err := os.MkdirTemp("", "keibidrop-bench-*")
	if err != nil {
		b.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	sizes := []int{1024, 10 * 1024, 100 * 1024, 1024 * 1024}

	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)

		b.Run(fmt.Sprintf("Write_%dKB", size/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				path := filepath.Join(tmpDir, fmt.Sprintf("bench_%d_%d.bin", size, i))
				if err := os.WriteFile(path, data, 0644); err != nil {
					b.Fatal(err)
				}
			}
		})

		// Create file for read benchmark
		readPath := filepath.Join(tmpDir, fmt.Sprintf("read_%d.bin", size))
		os.WriteFile(readPath, data, 0644)

		b.Run(fmt.Sprintf("Read_%dKB", size/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				if _, err := os.ReadFile(readPath); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFUSEMount measures FUSE overhead (requires mounted filesystem)
// Run with: go test -bench=BenchmarkFUSEMount -benchtime=10s ./tests/...
// Make sure MountAlice is mounted first!
func BenchmarkFUSEMount(b *testing.B) {
	mountPath := "../MountAlice"

	// Check if mount exists
	if _, err := os.Stat(mountPath); os.IsNotExist(err) {
		b.Skip("MountAlice not mounted, skipping FUSE benchmark")
	}

	sizes := []int{1024, 10 * 1024, 100 * 1024}

	for _, size := range sizes {
		data := make([]byte, size)
		rand.Read(data)

		b.Run(fmt.Sprintf("Write_%dKB", size/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				path := filepath.Join(mountPath, fmt.Sprintf("bench_%d_%d.bin", size, i))
				if err := os.WriteFile(path, data, 0644); err != nil {
					b.Fatal(err)
				}
				// Clean up
				os.Remove(path)
			}
		})

		// Create file for read benchmark
		readPath := filepath.Join(mountPath, fmt.Sprintf("read_%d.bin", size))
		os.WriteFile(readPath, data, 0644)
		defer os.Remove(readPath)

		b.Run(fmt.Sprintf("Read_%dKB", size/1024), func(b *testing.B) {
			b.SetBytes(int64(size))
			for i := 0; i < b.N; i++ {
				if _, err := os.ReadFile(readPath); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// MeasureLatency provides human-readable latency measurements
// Run with: go test -run=MeasureLatency -v ./tests/...
func TestMeasureLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping latency test in short mode")
	}

	mountPath := "../MountAlice"
	if _, err := os.Stat(mountPath); os.IsNotExist(err) {
		t.Skip("MountAlice not mounted, skipping latency test")
	}

	sizes := []struct {
		name string
		size int
	}{
		{"1KB", 1024},
		{"10KB", 10 * 1024},
		{"100KB", 100 * 1024},
		{"1MB", 1024 * 1024},
	}

	t.Log("\n=== FUSE Mount Latency Measurements ===\n")
	t.Logf("%-10s | %-15s | %-15s | %-15s\n", "Size", "Create+Write", "Read", "Total")
	t.Logf("%s\n", "----------|-----------------|-----------------|----------------")

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)
		path := filepath.Join(mountPath, fmt.Sprintf("latency_%s.bin", s.name))

		// Measure create + write
		startWrite := time.Now()
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Logf("%-10s | WRITE ERROR: %v\n", s.name, err)
			continue
		}
		writeLatency := time.Since(startWrite)

		// Small delay to ensure file is committed
		time.Sleep(100 * time.Millisecond)

		// Measure read
		startRead := time.Now()
		readData, err := os.ReadFile(path)
		if err != nil {
			t.Logf("%-10s | OK | READ ERROR: %v\n", s.name, err)
			os.Remove(path)
			continue
		}
		readLatency := time.Since(startRead)

		// Verify data
		if len(readData) != len(data) {
			t.Logf("%-10s | Size mismatch: wrote %d, read %d\n", s.name, len(data), len(readData))
		}

		t.Logf("%-10s | %-15s | %-15s | %-15s\n",
			s.name,
			writeLatency.Round(time.Microsecond),
			readLatency.Round(time.Microsecond),
			(writeLatency + readLatency).Round(time.Microsecond))

		// Cleanup
		os.Remove(path)
	}

	t.Log("\n=== Local Disk Baseline ===\n")
	tmpDir, _ := os.MkdirTemp("", "latency-baseline-*")
	defer os.RemoveAll(tmpDir)

	t.Logf("%-10s | %-15s | %-15s | %-15s\n", "Size", "Create+Write", "Read", "Total")
	t.Logf("%s\n", "----------|-----------------|-----------------|----------------")

	for _, s := range sizes {
		data := make([]byte, s.size)
		rand.Read(data)
		path := filepath.Join(tmpDir, fmt.Sprintf("baseline_%s.bin", s.name))

		startWrite := time.Now()
		os.WriteFile(path, data, 0644)
		writeLatency := time.Since(startWrite)

		startRead := time.Now()
		os.ReadFile(path)
		readLatency := time.Since(startRead)

		t.Logf("%-10s | %-15s | %-15s | %-15s\n",
			s.name,
			writeLatency.Round(time.Microsecond),
			readLatency.Round(time.Microsecond),
			(writeLatency + readLatency).Round(time.Microsecond))
	}
}

// TestThroughput measures sustained throughput
func TestThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping throughput test in short mode")
	}

	mountPath := "../MountAlice"
	if _, err := os.Stat(mountPath); os.IsNotExist(err) {
		t.Skip("MountAlice not mounted, skipping throughput test")
	}

	// 10MB test file
	size := 10 * 1024 * 1024
	data := make([]byte, size)
	rand.Read(data)

	path := filepath.Join(mountPath, "throughput_test.bin")
	defer os.Remove(path)

	t.Log("\n=== Throughput Test (10MB file) ===\n")

	// Write throughput
	start := time.Now()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	writeDuration := time.Since(start)
	writeMBps := float64(size) / writeDuration.Seconds() / 1024 / 1024

	t.Logf("Write: %.2f MB/s (%.2f seconds for 10MB)\n", writeMBps, writeDuration.Seconds())

	time.Sleep(500 * time.Millisecond)

	// Read throughput
	start = time.Now()
	readData, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	readDuration := time.Since(start)
	readMBps := float64(len(readData)) / readDuration.Seconds() / 1024 / 1024

	t.Logf("Read:  %.2f MB/s (%.2f seconds for 10MB)\n", readMBps, readDuration.Seconds())
}

// TestOpenCloseLatency measures file handle operations
func TestOpenCloseLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping open/close latency test in short mode")
	}

	mountPath := "../MountAlice"
	if _, err := os.Stat(mountPath); os.IsNotExist(err) {
		t.Skip("MountAlice not mounted, skipping latency test")
	}

	// Create a test file
	path := filepath.Join(mountPath, "openclose_test.txt")
	os.WriteFile(path, []byte("test content"), 0644)
	defer os.Remove(path)

	time.Sleep(500 * time.Millisecond)

	t.Log("\n=== Open/Close Latency (100 iterations) ===\n")

	iterations := 100
	var totalOpen, totalClose time.Duration

	for i := 0; i < iterations; i++ {
		startOpen := time.Now()
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("Open failed: %v", err)
		}
		totalOpen += time.Since(startOpen)

		startClose := time.Now()
		f.Close()
		totalClose += time.Since(startClose)
	}

	t.Logf("Average Open:  %v\n", totalOpen/time.Duration(iterations))
	t.Logf("Average Close: %v\n", totalClose/time.Duration(iterations))
	t.Logf("Total for %d iterations: Open=%v, Close=%v\n", iterations, totalOpen, totalClose)
}
