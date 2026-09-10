// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// The leg shape hints cross the handshake as advertised, and change nothing else:
// the keys still match, so a peer that reads them wrong can only pick a slower path.
func TestHandshake_LegShapeHintsCross(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	alice, err := InitSession(logger, 26012, 26011)
	require.NoError(t, err)
	bob, err := InitSession(logger, 26014, 26013)
	require.NoError(t, err)
	alicePeer, err := kbc.ParsePeerKeys(map[string][]byte{
		"x25519": alice.OwnKeys.X25519Public.Bytes(),
		"mlkem":  alice.OwnKeys.MlKemPublic.Bytes(),
	})
	require.NoError(t, err)
	bob.PeerPubKeys = alicePeer
	alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
	require.NoError(t, err)
	bob.OwnInboundBlocked = true
	bob.OwnMixedLegs = true

	bobEnd, aliceEnd := net.Pipe()
	defer bobEnd.Close()
	defer aliceEnd.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- performOutboundHandshake(bob, bobEnd, true) }()
	require.NoError(t, PerformInboundHandshake(alice, aliceEnd))
	require.NoError(t, <-errCh)

	require.True(t, alice.PeerInboundBlocked, "the joiner's blocked inbound reached the creator")
	require.True(t, alice.PeerMixedLegs, "the joiner's intent to keep its leg reached the creator")
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound, "the hints are not bound into the key")
}

// A peer that sends neither field (every release before this one) decodes as false.
func TestHandshake_LegShapeHintsDefaultOff(t *testing.T) {
	alice, msg := buildHandshakeFixture(t)
	alice.ExpectedPeerFingerprint = "TOFU"
	conn := pipeWithMessage(t, msg)
	defer conn.Close()
	require.NoError(t, PerformInboundHandshake(alice, conn))
	require.False(t, alice.PeerInboundBlocked)
	require.False(t, alice.PeerMixedLegs)
}

// A joiner that intends to keep the leg gets the creator's byte and answers it; the
// keys are unchanged by the exchange.
func TestHandshake_MixedLegAckRoundTrip(t *testing.T) {
	alice, bob := legTestPair(t)
	bob.OwnMixedLegs = true
	bob.OwnInboundBlocked = true
	bobEnd, aliceEnd := net.Pipe()
	defer bobEnd.Close()
	defer aliceEnd.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- performOutboundHandshake(bob, bobEnd, true) }()
	require.NoError(t, PerformInboundHandshake(alice, aliceEnd))
	require.NoError(t, <-errCh)
	require.True(t, alice.PeerMixedLegs)
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound)
	require.NotNil(t, alice.InboundConn(), "the creator installed the leg")
	require.NotNil(t, bob.OutboundConn(), "the joiner installed the leg")
}

// A dial that lands in an idle backlog (nobody accepts) gets no byte back: the
// joiner gives the leg up within the ack timeout instead of keeping a phantom.
func TestHandshake_JoinerGivesUpALegNobodyTook(t *testing.T) {
	orig := mixedAckTimeout
	mixedAckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mixedAckTimeout = orig })
	_, bob := legTestPair(t)
	bob.OwnMixedLegs = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close() // Listening, never accepting: the kernel completes the connect.
	conn, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	err = performOutboundHandshake(bob, conn, true)
	require.ErrorIs(t, err, ErrNoMixedAck)
	require.Nil(t, bob.OutboundConn(), "no phantom leg is installed")
}

// A stale dial pulled out of the backlog later (the joiner wrote its handshake and
// went away) fails the creator's handshake, so the accept loop closes it and
// accepts again instead of keeping a dead leg.
func TestHandshake_CreatorRefusesALegWhoseJoinerLeft(t *testing.T) {
	orig := mixedAckTimeout
	mixedAckTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mixedAckTimeout = orig })
	alice, bob := legTestPair(t)
	bob.OwnMixedLegs = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	// The joiner dials, writes its handshake, waits for a byte that never comes
	// (nobody accepts yet) and closes: exactly a stale backlog entry.
	go func() {
		c, dErr := net.Dial("tcp", ln.Addr().String())
		if dErr == nil {
			_ = performOutboundHandshake(bob, c, true) // returns ErrNoMixedAck and closes c
		}
	}()
	time.Sleep(400 * time.Millisecond) // let the joiner give up before the accept
	c, err := ln.Accept()
	require.NoError(t, err)
	defer c.Close()
	err = PerformInboundHandshake(alice, c)
	require.ErrorIs(t, err, ErrNoMixedAck)
	require.Nil(t, alice.InboundConn(), "a leg whose joiner left is not installed")
}

func legTestPair(t *testing.T) (alice, bob *Session) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var err error
	alice, err = InitSession(logger, 26022, 26021)
	require.NoError(t, err)
	bob, err = InitSession(logger, 26024, 26023)
	require.NoError(t, err)
	alicePeer, err := kbc.ParsePeerKeys(map[string][]byte{
		"x25519": alice.OwnKeys.X25519Public.Bytes(),
		"mlkem":  alice.OwnKeys.MlKemPublic.Bytes(),
	})
	require.NoError(t, err)
	bob.PeerPubKeys = alicePeer
	alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
	require.NoError(t, err)
	return alice, bob
}

// On a bridge leg the intent is not advertised and nothing is awaited, whatever the
// session's own flag says: the creator on that leg neither sends nor expects a byte.
func TestHandshake_BridgeLegCarriesNoAck(t *testing.T) {
	alice, bob := legTestPair(t)
	bob.OwnMixedLegs = true
	bob.OwnInboundBlocked = true
	bobEnd, aliceEnd := net.Pipe()
	defer bobEnd.Close()
	defer aliceEnd.Close()
	errCh := make(chan error, 1)
	go func() { errCh <- PerformOutboundHandshakeOnConn(bob, bobEnd) }()
	require.NoError(t, PerformInboundHandshake(alice, aliceEnd))
	require.NoError(t, <-errCh)
	require.False(t, alice.PeerMixedLegs, "a bridge leg never says it is kept")
	require.True(t, alice.PeerInboundBlocked, "the reachability hint still travels")
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound)
}
