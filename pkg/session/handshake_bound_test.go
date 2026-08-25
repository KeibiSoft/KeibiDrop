// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests the two-phase inbound handshake bound: a silent peer is rejected on the
// ABOUTME: short first-byte budget, a talking one gets the longer one, and success clears it.

package session

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shrinkHandshakeBounds makes the two budgets small enough to assert on without sleeping.
func shrinkHandshakeBounds(t *testing.T, firstByte, full time.Duration) {
	t.Helper()
	origFirst, origFull := firstByteTimeout, inboundHandshakeTimeout
	firstByteTimeout, inboundHandshakeTimeout = firstByte, full
	t.Cleanup(func() { firstByteTimeout, inboundHandshakeTimeout = origFirst, origFull })
}

// deadlineSpy records what the handshake does to the conn's deadlines.
type deadlineSpy struct {
	net.Conn
	deadlines []time.Time
}

func (c *deadlineSpy) SetDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return c.Conn.SetDeadline(t)
}

// A peer that connects and never speaks must be rejected on the SHORT budget, so an
// accept loop keeps enough of its window to serve a real peer afterwards.
func TestInboundHandshakeRejectsSilentPeerOnFirstByteBound(t *testing.T) {
	shrinkHandshakeBounds(t, 60*time.Millisecond, 5*time.Second)
	bob, err := InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- PerformInboundHandshake(bob, server) }()

	select {
	case err := <-done:
		require.Error(t, err, "a silent peer must fail the handshake")
		assert.Less(t, time.Since(start), 2*time.Second,
			"must fail on the first-byte budget, not the full one")
	case <-time.After(4 * time.Second):
		t.Fatal("inbound handshake hung on a silent peer")
	}
}

// Once the peer has actually spoken, the bound extends: a stall after the length prefix
// must survive well past the first-byte budget before failing.
func TestInboundHandshakeExtendsBoundOncePeerSpeaks(t *testing.T) {
	shrinkHandshakeBounds(t, 60*time.Millisecond, 700*time.Millisecond)
	bob, err := InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- PerformInboundHandshake(bob, server) }()
	// Announce a payload, then never send it.
	_, err = client.Write([]byte{0, 0, 0, 32})
	require.NoError(t, err)

	select {
	case err := <-done:
		require.Error(t, err, "a stalled payload must still fail")
		assert.Greater(t, time.Since(start), 300*time.Millisecond,
			"a talking peer must get the longer budget, not the first-byte one")
	case <-time.After(5 * time.Second):
		t.Fatal("inbound handshake hung after the length prefix")
	}
}

// The connection becomes the long-lived session transport, so a successful handshake
// MUST leave no deadline behind. If this regressed, every session would die at the bound.
func TestInboundHandshakeClearsDeadlineOnSuccess(t *testing.T) {
	shrinkHandshakeBounds(t, 2*time.Second, 5*time.Second)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	alice, err := InitSession(logger, 26002, 26001)
	require.NoError(t, err)
	bob, err := InitSession(logger, 26004, 26003)
	require.NoError(t, err)

	alicePeer, err := kbc.ParsePeerKeys(map[string][]byte{
		"x25519": alice.OwnKeys.X25519Public.Bytes(),
		"mlkem":  alice.OwnKeys.MlKemPublic.Bytes(),
	})
	require.NoError(t, err)
	bob.PeerPubKeys = alicePeer
	alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
	require.NoError(t, err)

	bobEnd, aliceEnd := net.Pipe()
	defer bobEnd.Close()
	defer aliceEnd.Close()

	spy := &deadlineSpy{Conn: aliceEnd}
	errCh := make(chan error, 1)
	go func() { errCh <- PerformOutboundHandshakeOnConn(bob, bobEnd) }()
	require.NoError(t, PerformInboundHandshake(alice, spy))
	require.NoError(t, <-errCh)

	require.NotEmpty(t, spy.deadlines, "the handshake must arm a bound")
	assert.True(t, spy.deadlines[len(spy.deadlines)-1].IsZero(),
		"the last deadline change on success must be the clear")
}

// Pins the shipped budgets so nobody quietly changes them.
func TestInboundHandshakeBoundDefaults(t *testing.T) {
	assert.Equal(t, 3*time.Second, firstByteTimeout)
	assert.Equal(t, 30*time.Second, inboundHandshakeTimeout)
}

// A bridge leg waits for a peer that may arrive well into the round, so the wait
// variant must take the caller's first-byte budget instead of the accept-site default.
func TestInboundHandshakeWaitAcceptsLateFirstByte(t *testing.T) {
	shrinkHandshakeBounds(t, 60*time.Millisecond, 5*time.Second)
	bob, err := InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- PerformInboundHandshakeWait(bob, server, 2*time.Second) }()

	// Arrive late: past the default first-byte budget, inside the caller's.
	time.Sleep(250 * time.Millisecond)
	go func() { _, _ = client.Write([]byte{0xff, 0xff, 0xff, 0xff}) }() // oversized length

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorContains(t, err, "too large",
			"the late first byte must be read, not killed by the default budget")
		assert.Greater(t, time.Since(start), 200*time.Millisecond,
			"failure must come from the oversized length, not the first-byte bound")
	case <-time.After(4 * time.Second):
		t.Fatal("wait-variant handshake hung")
	}
}
