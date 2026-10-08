// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: A funded session keeps paying across a reconnect: the reveal loop that ended when the
// ABOUTME: old legs closed starts again on the new legs' ack, once, and announces itself once.
package common

import (
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// paidLeg is a bridge leg that acks paid: the conn the session reads it from
// has taken the ack once Read returns.
func paidLeg(t *testing.T, ts *tokenSession) *payConn {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { server.Close() })
	go func() {
		_, _ = server.Write([]byte{ackPaidBit, 'x'})
		_, _ = io.Copy(io.Discard, server)
	}()
	pc := newPayConn(client, ts)
	buf := make([]byte, 1)
	_ = pc.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := pc.Read(buf)
	require.NoError(t, err)
	_ = pc.SetReadDeadline(time.Time{})
	return pc
}

func (l *miniLedger) left() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.remaining
}

func TestRevealLoop_RestartsAfterAReconnect(t *testing.T) {
	seed := testSeed(t)
	const units = 100
	led := &miniLedger{last: chainHashAt(seed, units), remaining: units}
	srv := httptest.NewServer(led.handler())
	t.Cleanup(srv.Close)

	kd := newTokenTestKD(t, srv.URL)
	var mu sync.Mutex
	var events []string
	kd.OnEvent = func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	_, err := kd.Wallet().Add(encodeTokenCode(seed, units))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	ts.tick = 10 * time.Millisecond
	t.Cleanup(kd.resetTokenSession)

	// First legs: 20 units moved, half is 10, plus the lead of 2.
	first := paidLeg(t, ts)
	ts.sent.Add(20 * TokenUnitBytes)
	require.Eventually(t, func() bool { return led.left() == units-12 }, 3*time.Second, 5*time.Millisecond,
		"the first legs never paid")

	// The reconnect: the old legs close and the loop, seeing none, ends.
	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return !ts.looping.Load() }, 3*time.Second, 5*time.Millisecond,
		"the loop outlived the legs")

	// The new legs ack paid again and move 20 more units: they must be paid for.
	second := paidLeg(t, ts)
	t.Cleanup(func() { second.Close() })
	ts.recv.Add(20 * TokenUnitBytes)
	require.Eventually(t, func() bool { return led.left() == units-22 }, 3*time.Second, 5*time.Millisecond,
		"the reconnected session never paid: the reveal loop did not start again (ledger left %d)", led.left())

	mu.Lock()
	defer mu.Unlock()
	inUse := 0
	for _, e := range events {
		if strings.HasPrefix(e, "tokens_in_use:") {
			inUse++
		}
	}
	require.Equal(t, 1, inUse, "tokens_in_use said %d times for one session: %v", inUse, events)
}

// Two legs that ack at once start one loop, not two.
func TestRevealLoop_OneLoopForBothLegs(t *testing.T) {
	seed := testSeed(t)
	kd := newTokenTestKD(t, "")
	_, err := kd.Wallet().Add(encodeTokenCode(seed, 10))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	ts.tick = time.Hour // the loop must not tick during the test
	t.Cleanup(kd.resetTokenSession)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ts.applyAck(ackPaidBit) }()
	}
	wg.Wait()
	require.True(t, ts.looping.Load())
	// A second start while one runs is refused.
	require.False(t, ts.looping.CompareAndSwap(false, true))
}

// A dry chain stops the loop for good: a later ack does not start it again.
func TestRevealLoop_DryChainStaysStopped(t *testing.T) {
	kd := newTokenTestKD(t, "")
	_, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 10))
	require.NoError(t, err)
	ts := kd.tokenSessionFor("bridge.keibisoft.com:26600", kd.logger)
	require.NotNil(t, ts)
	t.Cleanup(kd.resetTokenSession)
	ts.dry.Store(true)
	ts.applyAck(ackPaidBit)
	require.False(t, ts.looping.Load(), "a dry chain started a reveal loop")
	require.True(t, ts.paid.Load(), "the ack is still recorded")
}

// The position of a value is found in one pass: a 250 GiB pack resyncs in
// milliseconds, not the minute the quadratic walk took.
func TestChainPosition_OnePass(t *testing.T) {
	seed := testSeed(t)
	const units = 25600
	for _, pos := range []int{0, 1, 1023, 12800, units - 1, units} {
		v := chainHashAt(seed, pos)
		require.Equal(t, pos, chainPosition(seed, units, v[:]))
	}
	missing := chainHashAt(seed, units+1)
	start := time.Now()
	require.Equal(t, -1, chainPosition(seed, units, missing[:]))
	require.Less(t, time.Since(start), 2*time.Second, "one pass over 25600 units took too long")
}
