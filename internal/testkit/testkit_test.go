// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Unit tests for the testkit lift, wait, concurrency and stream helpers.
// ABOUTME: Covers the goroutine-leak and stable-timing guarantees.

package testkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/stretchr/testify/require"
)

var errBoom = errors.New("boom")

func TestRun_PassesOnANilError(t *testing.T) {
	Run(t, func() error { return nil })
}

// Must(f()) is legal because the call is the sole argument, and T infers.
func TestMust_ReturnsTheValueAndInfersTheType(t *testing.T) {
	Run(t, func() error {
		n := Must(func() (int, error) { return 42, nil }())
		s, b := Must2(func() (string, []byte, error) { return "s", []byte("bb"), nil }())
		return fp.All(
			fp.Equal("int", n, 42),
			fp.Equal("string", s, "s"),
			fp.Len("bytes", b, 2),
		)
	})
}

// A Must failure inside Run becomes an ordinary error, not a crash.
func TestMust_FailureIsRecoveredByRun(t *testing.T) {
	err := recovering(func() error {
		_ = Must(func() (int, error) { return 0, errBoom }())
		t.Error("Must must not return after an error")
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), errBoom.Error())
	// The caller location is carried in the message, because Must cannot call
	// t.Helper.
	require.Contains(t, err.Error(), "testkit_test.go:")
}

func TestMust2_FailureIsRecoveredByRun(t *testing.T) {
	err := recovering(func() error {
		_, _ = Must2(func() (int, string, error) { return 0, "", errBoom }())
		t.Error("Must2 must not return after an error")
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), errBoom.Error())
}

// A panic from the code under test becomes a readable failure with its stack,
// so the rest of the package still runs.
func TestRun_TurnsAnyPanicIntoAnError(t *testing.T) {
	err := recovering(func() error { panic("nil map write") })
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil map write")
	require.Contains(t, err.Error(), "recovering", "the stack must be attached")
}

func TestRun_ReturnsTheBodyError(t *testing.T) {
	require.ErrorIs(t, recovering(func() error { return errBoom }), errBoom)
}

func TestPoll_ReturnsImmediatelyWhenAlreadyTrue(t *testing.T) {
	start := time.Now()
	require.NoError(t, Poll(time.Second, 50*time.Millisecond, func() bool { return true }, "always"))
	require.Less(t, time.Since(start), 40*time.Millisecond, "Poll must not sleep before the first check")
}

func TestPoll_TimesOutAndNamesTheSubject(t *testing.T) {
	err := Poll(20*time.Millisecond, time.Millisecond, func() bool { return false }, "the thing")
	require.Error(t, err)
	require.Contains(t, err.Error(), "the thing")
	require.Contains(t, err.Error(), "timed out")
}

// The last sleep can cross the deadline, so Poll checks once more afterwards.
func TestPoll_ChecksOnceAfterTheDeadline(t *testing.T) {
	var flipped atomic.Bool
	time.AfterFunc(30*time.Millisecond, func() { flipped.Store(true) })
	require.NoError(t, Poll(40*time.Millisecond, 35*time.Millisecond, flipped.Load, "flip"))
}

func TestPoll_NilConditionIsAnError(t *testing.T) {
	require.Error(t, Poll(time.Millisecond, time.Millisecond, nil, "x"))
}

func TestGo_ReturnsTheErrorThroughTheJoin(t *testing.T) {
	join := Go(func() error { return errBoom })
	require.ErrorIs(t, join(), errBoom)

	ok := Go(func() error { return nil })
	require.NoError(t, ok())

	require.Error(t, Go(nil)())
}

// The join function is a func() error, so it composes with fp.All unwrapped.
func TestGo_ComposesIntoAll(t *testing.T) {
	join := Go(func() error { return nil })
	require.NoError(t, fp.All(
		fp.Equal("a", 1, 1),
		join(),
	))
}

func TestGoN_ReportsTheLowestFailingIndex(t *testing.T) {
	err := GoN(5, func(i int) error {
		if i == 1 || i == 3 {
			return errBoom
		}
		return nil
	})()
	require.Error(t, err)
	require.Contains(t, err.Error(), "worker 1")

	require.NoError(t, GoN(4, func(int) error { return nil })())
	require.NoError(t, GoN(0, func(int) error { return errBoom })())
	require.Error(t, GoN(1, nil)())
}

// GoN must drain every result, so a failing worker does not leak the others.
func TestGoN_DoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		_ = GoN(8, func(i int) error {
			if i == 0 {
				return errBoom
			}
			time.Sleep(time.Millisecond)
			return nil
		})()
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+2 {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before+2,
		"goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestWithin_PassesTheBodyResultThrough(t *testing.T) {
	require.NoError(t, Within(2*time.Second, "quick", func() error { return nil }))
	require.ErrorIs(t, Within(2*time.Second, "quick", func() error { return errBoom }), errBoom)
	require.Error(t, Within(time.Second, "nil body", nil))
}

func TestWithin_TimesOutAndNamesTheSubject(t *testing.T) {
	err := Within(20*time.Millisecond, "slow handshake", func() error {
		time.Sleep(2 * time.Second)
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "slow handshake")
	require.Contains(t, err.Error(), "did not finish")
}

// The reason Within owns the goroutine. Must panics, and only the goroutine
// that panics can recover it, so a Must in a bare go func ends the binary.
func TestWithin_RecoversAMustSoTheBinarySurvives(t *testing.T) {
	err := Within(2*time.Second, "body with a failing Must", func() error {
		_ = Must(0, errBoom)
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "panic")
	require.Contains(t, err.Error(), errBoom.Error())
}

// A body that finishes late must not block the goroutine forever.
func TestWithin_LateBodyDoesNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	release := make(chan struct{})
	for i := 0; i < 20; i++ {
		require.Error(t, Within(time.Millisecond, "late", func() error {
			<-release
			return nil
		}))
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > before+2 {
		time.Sleep(10 * time.Millisecond)
	}
	require.LessOrEqual(t, runtime.NumGoroutine(), before+2,
		"goroutines leaked: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestWithinCtx_PassesTheBodyResultThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, WithinCtx(ctx, "quick", func() error { return nil }))
	require.ErrorIs(t, WithinCtx(ctx, "quick", func() error { return errBoom }), errBoom)
	require.Error(t, WithinCtx(ctx, "nil body", nil))
	require.Error(t, WithinCtx(nil, "nil ctx", func() error { return nil })) //nolint:staticcheck // the nil guard is the subject
}

func TestWithinCtx_ReportsTheContextError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := WithinCtx(ctx, "slow handshake", func() error {
		time.Sleep(2 * time.Second)
		return nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Contains(t, err.Error(), "slow handshake")
}

// The point of WithinCtx: one context deadline is shared by every wait under
// it. Two Within calls of the same duration would give each wait its own
// budget, so the pair would be allowed twice the total time.
func TestWithinCtx_SharesOneDeadlineAcrossWaits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	block := func() error { time.Sleep(5 * time.Second); return nil }

	require.Error(t, WithinCtx(ctx, "first", block)) // spends the whole deadline
	start := time.Now()
	require.Error(t, WithinCtx(ctx, "second", block))
	require.Less(t, time.Since(start), 100*time.Millisecond,
		"the second wait must see an expired deadline, not restart its own")
}

func TestWithinCtx_RecoversAMustSoTheBinarySurvives(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := WithinCtx(ctx, "body with a failing Must", func() error {
		_ = Must(0, errBoom)
		return nil
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "panic")
	require.Contains(t, err.Error(), errBoom.Error())
}

func TestRandBytes_IsNotAllZero(t *testing.T) {
	b := RandBytes(t, 64)
	require.Len(t, b, 64)
	var zero [64]byte
	require.NotEqual(t, zero[:], b)
}

func TestDiscardLogger_Works(t *testing.T) {
	require.NotNil(t, DiscardLogger())
	DiscardLogger().Info("this must not print")
}

// Logger keeps the output. A test that logs for a human to read after a failure
// must not be converted to DiscardLogger.
func TestLogger_WritesAndHonoursTheLevel(t *testing.T) {
	var buf bytes.Buffer
	log := Logger(&buf, slog.LevelWarn)

	log.Info("dropped below the level")
	require.Empty(t, buf.String())

	log.Warn("kept", "k", "v")
	out := buf.String()
	require.Contains(t, out, "kept")
	require.Contains(t, out, "k=v")

	log.Error("also kept")
	require.Contains(t, buf.String(), "also kept")
}

func TestStdoutLogger_IsNotNil(t *testing.T) {
	require.NotNil(t, StdoutLogger(slog.LevelWarn))
}

func TestRunTable_ProducesOneSubtestPerCase(t *testing.T) {
	seen := make([]string, 0, 3)
	cases := []NamedSize{{"a", 1}, {"b", 2}, {"c", 3}}
	RunTable(t, cases, func(c NamedSize) string { return c.Name }, func(t *testing.T, c NamedSize) error {
		seen = append(seen, c.Name)
		return nil
	})
	require.Equal(t, []string{"a", "b", "c"}, seen)
}

func TestStdSizes_MatchesTheSharedLadder(t *testing.T) {
	require.Len(t, StdSizes, 4)
	require.Equal(t, "1MB", StdSizes[0].Name)
	require.Equal(t, 1*1024*1024, StdSizes[0].Size)
	require.Equal(t, "1GB", StdSizes[3].Name)
	require.Equal(t, 1024*1024*1024, StdSizes[3].Size)
}

type req struct{ n int }
type resp struct{ s string }

func TestStream_RecordsSendsAndReplaysRecvs(t *testing.T) {
	s := &Stream[req, resp]{Requests: []req{{1}, {2}}}

	require.NoError(t, s.Send(resp{"a"}))
	require.NoError(t, s.Send(resp{"b"}))
	require.Equal(t, []resp{{"a"}, {"b"}}, s.Sent)

	r1, err := s.Recv()
	require.NoError(t, err)
	require.Equal(t, 1, r1.n)
	r2, err := s.Recv()
	require.NoError(t, err)
	require.Equal(t, 2, r2.n)
	_, err = s.Recv()
	require.ErrorIs(t, err, io.EOF)
}

func TestStream_InjectedErrors(t *testing.T) {
	s := &Stream[req, resp]{SendErr: errBoom, RecvErr: errBoom}
	require.ErrorIs(t, s.Send(resp{}), errBoom)
	_, err := s.Recv()
	require.ErrorIs(t, err, errBoom)
	require.Empty(t, s.Sent, "a failed Send must not record")
}

// The grpc.ServerStream methods must not panic. The hand-written stubs this
// replaces implement them directly for the same reason.
func TestStream_SatisfiesTheServerStreamMethods(t *testing.T) {
	s := &Stream[req, resp]{}
	require.NotNil(t, s.Context())
	require.NoError(t, s.SetHeader(nil))
	require.NoError(t, s.SendHeader(nil))
	s.SetTrailer(nil)
	require.NoError(t, s.SendMsg(nil))
	require.NoError(t, s.RecvMsg(nil))
}
