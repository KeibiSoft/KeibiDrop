// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests for auto_connect_peer — contact resolution and the connect watchdog.
// ABOUTME: Covers unknown-contact rejection, backoff retry, and rearm after teardown.

package common

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
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

// A peer that said goodbye leaves no reconnect to defer to: the loop redials
// on the next poll instead of sitting out the rearm grace, and a new session
// clears the flag.
func TestAutoConnectLoop_RedialsAtOnceAfterPeerGoodbye(t *testing.T) {
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
	slow := fastTuning
	slow.rearmGrace = 5 * time.Second

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, slow, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 1 }, "first dial")
	waitFor(t, 2*time.Second, running.Load, "session up")

	kd.peerSaidGoodbye.Store(true)
	running.Store(false)
	waitFor(t, time.Second, func() bool { return dials.Load() == 2 }, "re-dial without the rearm grace")
	waitFor(t, time.Second, func() bool { return !kd.peerSaidGoodbye.Load() }, "new session clears the goodbye flag")

	cancel()
	<-done
}

// The goodbye arrives a moment before the session ends. Polls that land in that
// gap see the session still running; they must not clear the goodbye, or the
// loop waits the full rearm grace (measured on the box: 95 s instead of 8).
func TestAutoConnectLoop_GoodbyeSurvivesPollsBeforeTheSessionEnds(t *testing.T) {
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
	slow := fastTuning
	slow.rearmGrace = 5 * time.Second

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, slow, dial, running.Load, func() bool { return false })
		close(done)
	}()
	waitFor(t, 2*time.Second, running.Load, "session up")

	kd.peerSaidGoodbye.Store(true)
	time.Sleep(20 * slow.poll) // many running polls between the goodbye and the end
	require.True(t, kd.peerSaidGoodbye.Load(), "a running poll must not swallow the goodbye")
	running.Store(false)
	waitFor(t, time.Second, func() bool { return dials.Load() == 2 }, "re-dial without the rearm grace")

	cancel()
	<-done
}

// TestAutoConnectLoop_DefersWhileAnotherConnectRuns: a dial refused because a
// connect is already in flight (a click, an agent) is not a failed dial. The
// loop polls again at once and the backoff does not grow, so the redial after
// that connect ends is not held for minutes.
func TestAutoConnectLoop_DefersWhileAnotherConnectRuns(t *testing.T) {
	kd := newBareKD()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tun := autoConnectTuning{
		poll:           2 * time.Millisecond,
		initialBackoff: 100 * time.Millisecond,
		maxBackoff:     400 * time.Millisecond,
		rearmGrace:     20 * time.Millisecond,
	}
	var dials atomic.Int32
	var running atomic.Bool
	dial := func() error {
		if dials.Add(1) <= 5 {
			return ErrConnectInProgress
		}
		running.Store(true)
		return nil
	}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		kd.autoConnectLoop(ctx, tun, dial, running.Load, func() bool { return false })
		close(done)
	}()

	waitFor(t, 2*time.Second, running.Load, "the dial after the refusals succeeds")
	// Five refusals on the backoff would have cost 100+200+400+400+400 ms.
	require.Less(t, time.Since(start), 500*time.Millisecond, "refusals must not grow the backoff")
	require.Equal(t, int32(6), dials.Load())

	cancel()
	<-done
}

// TestStartAutoConnect_OneLoopThatAnnouncesItsEnd: a second arm while one
// runs starts no second loop, and when the loop ends with its context it
// says so and drops the armed flag, so the state line and a later re-arm see
// the truth. On 0.4.8 the desktop app's first disconnect ended the loop
// silently and the flag kept promising "will keep trying".
func TestStartAutoConnect_OneLoopThatAnnouncesItsEnd(t *testing.T) {
	var logs bytes.Buffer
	kd := newAutoConnectTestKD(t)
	kd.logger = testkit.Logger(&logs, slog.LevelDebug)
	kd.AutoConnectPeer = "dataset-box"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	require.NoError(t, kd.StartAutoConnect(ctx))
	require.NoError(t, kd.StartAutoConnect(ctx), "a second arm is a no-op, not an error")
	require.True(t, kd.AutoConnectArmed())

	cancel()
	waitFor(t, 2*time.Second, func() bool { return !kd.AutoConnectArmed() }, "the loop drops the armed flag when it ends")
	out := logs.String()
	require.Equal(t, 1, strings.Count(out, "Auto-connect armed"), "one loop per process")
	require.Contains(t, out, "Auto-connect already armed")
	require.Contains(t, out, "Auto-connect watchdog stopped")
}

// TestAutoConnectLoop_PausedUntilASessionExists: a person who cancelled the
// watchdog's dial has spoken. The loop does not redial until a session exists
// again, and then follows the next drop as before.
func TestAutoConnectLoop_PausedUntilASessionExists(t *testing.T) {
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
	kd.autoConnectArmed.Store(true)
	kd.PauseAutoConnect()
	require.True(t, kd.autoConnectPaused.Load())

	done := make(chan struct{})
	go func() {
		kd.autoConnectLoop(ctx, fastTuning, dial, running.Load, func() bool { return false })
		close(done)
	}()

	time.Sleep(30 * fastTuning.poll)
	require.Zero(t, dials.Load(), "paused: no dial without a session")

	// A manual connect made a session: the pause lifts, the drop is followed.
	running.Store(true)
	waitFor(t, 2*time.Second, func() bool { return !kd.autoConnectPaused.Load() }, "a session lifts the pause")
	running.Store(false)
	waitFor(t, 2*time.Second, func() bool { return dials.Load() == 1 }, "redial after the rearm grace")

	cancel()
	<-done
}
