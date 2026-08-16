// ABOUTME: Measurement probes for the confirmed kd.session teardown race. Env-gated so they
// ABOUTME: never run in make test; a driver runs each as a one-shot subprocess to count crashes.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/types"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// probeGate skips the probe unless KD_RACEPROBE=1, so these never fire in make test/test-race.
func probeGate(t *testing.T) {
	if os.Getenv("KD_RACEPROBE") != "1" {
		t.Skip("measurement probe; set KD_RACEPROBE=1 to run")
	}
}

// TestProbe_FlushTeardownNilDerefCrash is ONE crash trial: the notify worker's lock-free flush
// read of kd.session races a writer nil-ing and restoring it (as teardown does), so a mistimed
// read nil-derefs and crashes the process. A driver runs it many times; iters from KD_RACEPROBE_ITERS (default 200000).
func TestProbe_FlushTeardownNilDerefCrash(t *testing.T) {
	probeGate(t)
	iters := envIntDefault("KD_RACEPROBE_ITERS", 200000)

	kd := newTestKD(t)
	rel := make(chan struct{})
	close(rel)
	fake := &blockingNotifyClient{entered: make(chan struct{}, 1), release: rel}
	kd.session.GRPCClient = fake
	sessA := kd.session

	require.NoError(t, kd.setupFilesystem(kd.logger, nil), "setupFilesystem")

	var start, done sync.WaitGroup
	start.Add(1)

	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.FS.OnLocalChange(types.FileEvent{Action: types.RemoveFile, Path: "gone.txt"})
		}
	}()

	// Writer nils kd.session and restores it (as teardown does), unlocked: teardown's kd.mu
	// does not synchronize the lock-free flush reader, so this is the faithful worst case.
	done.Add(1)
	go func() {
		defer done.Done()
		start.Wait()
		for i := 0; i < iters; i++ {
			kd.session = nil
			kd.session = sessA
		}
	}()

	start.Done()
	done.Wait()
}

// TestProbe_OnReconnectedTeardownStackRace: with tearingDown set (teardown begun), the real
// onReconnected must bail at the guard without touching kd.grpcServer, so -race stays clean.
func TestProbe_OnReconnectedTeardownStackRace(t *testing.T) {
	probeGate(t)
	kd := newTestKD(t)
	kd.grpcServer = grpc.NewServer()

	// Run's teardown sets this before niling the stack; model that ordering.
	kd.tearingDown.Store(true)

	var start sync.WaitGroup
	start.Add(1)
	done := make(chan struct{})

	// Reader: the REAL onReconnected. With tearingDown set it must bail before the stack.
	go func() {
		start.Wait()
		defer func() { _ = recover() }()
		kd.onReconnected()
		close(done)
	}()

	// Writer rewrites kd.grpcServer; if onReconnected honors the guard it never reads this field.
	go func() {
		start.Wait()
		defer func() { _ = recover() }()
		for i := 0; i < 100000; i++ {
			kd.grpcServer = nil
			kd.grpcServer = grpc.NewServer()
		}
	}()

	start.Done()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("onReconnected did not bail on tearingDown")
	}
}

// TestProbe_OnReconnectedStackSwapRace: onReconnected's gRPC-stack swap (not tearing down) runs
// concurrent with teardown swapping the same fields; both swap under kd.mu, so -race stays clean.
func TestProbe_OnReconnectedStackSwapRace(t *testing.T) {
	probeGate(t)
	kd := newTestKD(t)
	kd.grpcServer = grpc.NewServer()

	var start sync.WaitGroup
	start.Add(1)

	go func() {
		start.Wait()
		defer func() { _ = recover() }()
		kd.onReconnected() // swaps the stack under kd.mu early, then blocks in connect
	}()
	// Writer models Run's teardown swapping the gRPC stack under kd.mu.
	go func() {
		start.Wait()
		defer func() { _ = recover() }()
		for i := 0; i < 200000; i++ {
			kd.mu.Lock()
			kd.grpcServer = nil
			kd.grpcClientConn = nil
			kd.KDClient = nil
			kd.mu.Unlock()
			kd.mu.Lock()
			kd.grpcServer = grpc.NewServer()
			kd.mu.Unlock()
		}
	}()

	start.Done()
	time.Sleep(300 * time.Millisecond)
}

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
