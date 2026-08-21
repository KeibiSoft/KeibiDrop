// ABOUTME: Measurement probe for the daemonDisc/localMyName/discoveredPeerMap lifecycle race.
// ABOUTME: Env-gated so it never runs in make test; models cmdDiscover's body minus its sleeps.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/discovery"
)

// probeGate skips the probe unless KD_RACEPROBE=1, so it never fires in make test/test-race.
func probeGate(t *testing.T) {
	if os.Getenv("KD_RACEPROBE") != "1" {
		t.Skip("measurement probe; set KD_RACEPROBE=1 to run")
	}
}

// envIntDefault reads a positive integer from the named env var, falling back to def.
func envIntDefault(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	if n == 0 {
		return def
	}
	return n
}

// TestProbe_DaemonDiscLifecycleRace is cmdDiscover's real sleeps (2-10s per call) make
// high-iteration replay infeasible, so this models the exact unsynchronized read/write pattern
// directly: cmdDiscover's lazy-init-and-use body, disconnect/stop's nil-out, and register's map
// read, minus the sleeps and the real UDP Start(). A driver runs this many times; iters from
// KD_RACEPROBE_ITERS (default 50000).
func TestProbe_DaemonDiscLifecycleRace(t *testing.T) {
	probeGate(t)
	iters := envIntDefault("KD_RACEPROBE_ITERS", 50000)
	svc := discovery.New(26999, slog.Default()) // never Started; Peers()/Name() don't need it

	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup

	wg.Add(1) // models cmdDiscover's racy body, minus the sleeps
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }()
		start.Wait()
		for i := 0; i < iters; i++ {
			// Snapshot-then-use under discMu, mirroring cmdDiscover's fix: the lock never
			// spans a discovery.Service call, only the variable touches around it.
			discMu.Lock()
			if daemonDisc == nil {
				daemonDisc = svc
			}
			disc := daemonDisc
			discMu.Unlock()

			_ = disc.Peers()
			name := disc.Name()

			// Reassign-then-populate, matching cmdDiscover's real shape (main.go:817,821),
			// both under one lock so goroutine 3's guarded read can never land mid-insert.
			discMu.Lock()
			localMyName = name
			discoveredPeerMap = make(map[string]string, 1)
			discoveredPeerMap["x"] = "y"
			discMu.Unlock()
		}
	}()

	wg.Add(1) // models disconnect/stop
	go func() {
		defer wg.Done()
		defer func() { _ = recover() }()
		start.Wait()
		for i := 0; i < iters; i++ {
			discMu.Lock()
			if daemonDisc != nil {
				daemonDisc = nil
			}
			discMu.Unlock()
		}
	}()

	wg.Add(1) // models register's map read; a fatal concurrent map read/write cannot be
	// recovered, so this goroutine deliberately has no recover() to mask that outcome.
	go func() {
		defer wg.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			discMu.Lock()
			_ = discoveredPeerMap["x"]
			discMu.Unlock()
		}
	}()

	start.Done()
	wg.Wait()
}
