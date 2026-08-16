// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests for auto_connect_peer — contact resolution and the connect watchdog.
// ABOUTME: Covers unknown-contact rejection, backoff retry, and rearm after teardown.

package common

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
	"github.com/stretchr/testify/require"
)

// newBareKD builds a KeibiDrop with a discard logger and nothing else, for
// tests that exercise a single method without the full NewKeibiDrop startup
// path. Each option sets one field, so a test states only what it depends on.
//
// Not named newTestKD: types_test.go already defines newTestKD(t) for the
// real NewKeibiDrop constructor, a different, heavier helper used elsewhere
// in this package.
func newBareKD(opts ...func(*KeibiDrop)) *KeibiDrop {
	kd := &KeibiDrop{logger: testkit.DiscardLogger()}
	for _, o := range opts {
		o(kd)
	}
	return kd
}

func newAutoConnectTestKD(t *testing.T) *KeibiDrop {
	t.Helper()
	ab, err := identity.LoadAddressBook(t.TempDir(), nil) // no file yet: empty book
	require.NoError(t, err)
	require.NoError(t, ab.Add("dataset-box", "FP-AAAA-BBBB"))
	kd := newBareKD()
	kd.AddressBook = ab
	return kd
}

func TestResolveContact_ByNameAndFingerprint(t *testing.T) {
	kd := newAutoConnectTestKD(t)

	fp, err := kd.ResolveContact("dataset-box")
	require.NoError(t, err)
	require.Equal(t, "FP-AAAA-BBBB", fp)

	fp, err = kd.ResolveContact("DATASET-BOX") // case-insensitive name
	require.NoError(t, err)
	require.Equal(t, "FP-AAAA-BBBB", fp)

	fp, err = kd.ResolveContact("FP-AAAA-BBBB") // fingerprint form
	require.NoError(t, err)
	require.Equal(t, "FP-AAAA-BBBB", fp)
}

func TestResolveContact_RejectsUnknown(t *testing.T) {
	kd := newAutoConnectTestKD(t)
	_, err := kd.ResolveContact("stranger")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a saved contact")
}

func TestResolveContact_RejectsIncognito(t *testing.T) {
	kd := newBareKD()
	_, err := kd.ResolveContact("anyone")
	require.Error(t, err)
	require.Contains(t, err.Error(), "persistent identity")
}

func TestStartAutoConnect_EmptyIsNoop(t *testing.T) {
	kd := newBareKD()
	require.NoError(t, kd.StartAutoConnect(context.Background()))
}

// fastTuning keeps watchdog tests in milliseconds.
var fastTuning = autoConnectTuning{
	poll:           2 * time.Millisecond,
	initialBackoff: 2 * time.Millisecond,
	maxBackoff:     10 * time.Millisecond,
	rearmGrace:     20 * time.Millisecond,
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timeout: " + msg)
}

// TestAutoConnectLoop_RetriesUntilPeerAppears is the offline-then-appears case:
// dials fail while the peer is away, then one succeeds and the loop goes idle.
func TestAutoConnectLoop_RetriesUntilPeerAppears(t *testing.T) {
	kd := newBareKD()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error {
		n := dials.Add(1)
		if n < 3 {
			return errors.New("peer offline")
		}
		running.Store(true)
		return nil
	}

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, fastTuning, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 3 && running.Load() }, "three dials then success")
	// Idle while running: no further dials.
	time.Sleep(20 * fastTuning.poll)
	require.Equal(t, int32(3), dials.Load())

	cancel()
	<-done
}

// TestAutoConnectLoop_RearmsAfterTeardown: after a session existed and went
// away with no reconnect in progress, the loop waits the grace and dials again.
func TestAutoConnectLoop_RearmsAfterTeardown(t *testing.T) {
	kd := newBareKD()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error {
		dials.Add(1)
		running.Store(true)
		return nil
	}

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, fastTuning, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 1 }, "first dial")
	waitFor(t, 2*time.Second, running.Load, "session up")

	// Peer disconnects for good: session gone, no reconnect running.
	running.Store(false)
	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 2 }, "re-dial after rearm grace")

	cancel()
	<-done
}

// TestAutoConnectLoop_DefersToReconnectManager: while reconnect owns the
// session the watchdog must not dial, even past the rearm grace.
func TestAutoConnectLoop_DefersToReconnectManager(t *testing.T) {
	kd := newBareKD()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dials atomic.Int32
	var running, reconnecting atomic.Bool
	dial := func() error {
		dials.Add(1)
		running.Store(true)
		return nil
	}

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, fastTuning, dial, running.Load, reconnecting.Load)
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 1 }, "first dial")

	// Transient drop: reconnect machinery takes over.
	reconnecting.Store(true)
	running.Store(false)
	time.Sleep(5 * fastTuning.rearmGrace)
	require.Equal(t, int32(1), dials.Load(), "watchdog must not dial while reconnect is busy")

	// Reconnect finishes the recovery itself.
	running.Store(true)
	reconnecting.Store(false)
	time.Sleep(10 * fastTuning.poll)
	require.Equal(t, int32(1), dials.Load())

	cancel()
	<-done
}
