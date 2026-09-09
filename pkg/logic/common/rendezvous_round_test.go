// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file drives the creator's real rendezvous round across a round boundary
// with a real joiner handshake, and pins the joiner's retry of a direct dial
// that failed fast: the two halves of the fix for BUGS 33. Before it, a dial
// that met the boundary was reset or refused and the whole session went to
// the bridge.

package common

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// roundTestLogger is silent unless KD_TEST_LOG=1, which shows the engine's own
// lines while a round test is being read.
func roundTestLogger() *slog.Logger {
	if os.Getenv("KD_TEST_LOG") == "1" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return testkit.DiscardLogger()
}

// rendezvousPair returns a creator and a joiner that know each other's keys,
// as the relay registration would have told them.
func rendezvousPair(t *testing.T) (creator, joiner *session.Session) {
	t.Helper()
	logger := roundTestLogger()
	var err error
	creator, err = session.InitSession(logger, 26042, 26041)
	require.NoError(t, err)
	joiner, err = session.InitSession(logger, 26044, 26043)
	require.NoError(t, err)
	keysOf := func(s *session.Session) *kbc.PeerKeys {
		k, err := kbc.ParsePeerKeys(map[string][]byte{
			"x25519": s.OwnKeys.X25519Public.Bytes(),
			"mlkem":  s.OwnKeys.MlKemPublic.Bytes(),
		})
		require.NoError(t, err)
		return k
	}
	creator.PeerPubKeys = keysOf(joiner)
	joiner.PeerPubKeys = keysOf(creator)
	creator.ExpectedPeerFingerprint, err = joiner.OwnKeys.Fingerprint()
	require.NoError(t, err)
	joiner.ExpectedPeerFingerprint, err = creator.OwnKeys.Fingerprint()
	require.NoError(t, err)
	creator.PeerPort = 26043
	return creator, joiner
}

// silentBridge accepts every leg and reads whatever arrives without answering,
// which is what the bridge does while the other half of a room is missing.
func silentBridge(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	return ln.Addr().String()
}

// rendezvousKD is a creator with an open listener and a bridge, as CreateRoom
// leaves it before the first round.
func rendezvousKD(t *testing.T, creator *session.Session, bridge string) (*KeibiDrop, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	kd := newBareKD()
	kd.logger = roundTestLogger()
	kd.wallet = &TokenWallet{}
	kd.session = creator
	kd.listener = ln
	kd.inboundPort = ln.Addr().(*net.TCPAddr).Port
	kd.LocalIPv6IP = "2001:db8::1"
	kd.BridgeAddr = bridge
	kd.ctx = context.Background()
	return kd, ln
}

// A joiner that dials between two rounds lands in the listener's backlog with
// its handshake bytes; the next round takes it, acknowledges the leg and
// finishes as direct-in. The listener is never closed on the way.
func TestRendezvousRound_DialInTheGapLandsNextRound(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 300 * time.Millisecond
	defer func() { bridgeRoundWait = origWait }()

	creator, joiner := rendezvousPair(t)
	joiner.OwnMixedLegs = true      // keeps its leg, so it waits for the acknowledgement
	joiner.OwnInboundBlocked = true // the creator skips the dial back and takes pair2
	kd, ln := rendezvousKD(t, creator, silentBridge(t))

	done, err := kd.createRendezvousRound(kd.logger, 0)
	require.NoError(t, err)
	require.False(t, done, "round 0 had nobody")
	require.NotNil(t, kd.listener, "the listener stays open between rounds")

	// The gap between rounds: the joiner dials and sends its handshake.
	dialErr := make(chan error, 1)
	go func() { dialErr <- session.PerformOutboundHandshake(joiner, ln.Addr().String()) }()
	time.Sleep(150 * time.Millisecond) // so the dial is in the backlog before the round starts

	done, err = kd.createRendezvousRound(kd.logger, 1)
	require.NoError(t, err)
	require.True(t, done, "round 1 took the dial from the backlog")
	select {
	case err := <-dialErr:
		require.NoError(t, err, "the joiner got the leg acknowledgement")
	case <-time.After(5 * time.Second):
		t.Fatal("the joiner never got the acknowledgement")
	}
	require.Equal(t, ModeDirectIn, kd.ConnectionMode)
	require.NotNil(t, creator.InboundConn(), "the direct leg is the session's inbound")
	require.NotNil(t, joiner.OutboundConn())
	require.NotNil(t, kd.listener)
}

// The joiner's side of the same boundary: a dial that fails fast (reset,
// closed before the acknowledgement) is dialed again, and the next attempt
// completes the handshake.
func TestDialPeerDirect_RetriesAFastFailure(t *testing.T) {
	origWait := directDialRetryWait
	directDialRetryWait = 20 * time.Millisecond
	defer func() { directDialRetryWait = origWait }()

	creator, joiner := rendezvousPair(t)
	joiner.OwnMixedLegs = true
	joiner.OwnInboundBlocked = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	var accepts atomic.Int32
	go func() {
		c1, err := ln.Accept()
		if err != nil {
			return
		}
		accepts.Add(1)
		_ = c1.Close() // the old round boundary: reset before anyone read it
		c2, err := ln.Accept()
		if err != nil {
			return
		}
		accepts.Add(1)
		_ = session.PerformInboundHandshake(creator, c2) // the next round answers
	}()

	kd := newBareKD()
	kd.session = joiner
	require.NoError(t, kd.dialPeerDirect(kd.logger, []string{ln.Addr().String()}))
	require.Equal(t, int32(2), accepts.Load(), "one reset, one handshake")
	require.Equal(t, "127.0.0.1", kd.peerDialedIP.Load())
	require.NotNil(t, joiner.OutboundConn())
}

// Fast failures are bounded: after the attempts are spent the error stands,
// and a refused port costs the same bounded number of attempts.
func TestDialPeerDirect_FastFailuresAreBounded(t *testing.T) {
	origWait := directDialRetryWait
	directDialRetryWait = 10 * time.Millisecond
	defer func() { directDialRetryWait = origWait }()

	_, joiner := rendezvousPair(t)
	joiner.OwnMixedLegs = true
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close()
		}
	}()
	kd := newBareKD()
	kd.session = joiner
	require.Error(t, kd.dialPeerDirect(kd.logger, []string{ln.Addr().String()}))
	require.Equal(t, int32(directDialAttempts), accepts.Load())

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := closed.Addr().String()
	_ = closed.Close()
	began := time.Now()
	require.Error(t, kd.dialPeerDirect(kd.logger, []string{addr}), "nothing listens there")
	require.Less(t, time.Since(began), 2*time.Second, "a refused dial never waits out a timeout")
}
