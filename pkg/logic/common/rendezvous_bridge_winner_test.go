// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file drives the creator's real rendezvous round through a bridge that
// pairs legs by room token, with the joiner handshaking on the far end, and
// pins what the round leaves behind for each kind of winner. Found in cold
// install run 1 (2026-09-15): both peers behind a blocked inbound, the joiner
// arrived on the bridge leg, and the round closed that leg as though another
// leg had won. The bridge read EOF from the creator and dropped the joiner's
// outbound with it, so the joiner's gRPC client was built on a socket that was
// already gone and every heartbeat failed from the first tick.

package common

import (
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// tokenBridge stands in for the bridge between one creator and one joiner: it
// accepts the creator's legs, reads the room token off each and hands the conn
// over by direction, which is how the real bridge pairs rooms. The joiner is
// the test itself, so it runs its half of each handshake on the accepted end.
type tokenBridge struct {
	addr  string
	pair1 chan net.Conn // the creator's inbound leg, the joiner's outbound
	pair2 chan net.Conn // the creator's outbound leg, the joiner's inbound
	other chan net.Conn // a leg with a token neither direction expects
}

func newTokenBridge(t *testing.T, creator *session.Session) *tokenBridge {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	b := &tokenBridge{
		addr:  ln.Addr().String(),
		pair1: make(chan net.Conn, 4),
		pair2: make(chan net.Conn, 4),
		other: make(chan net.Conn, 4),
	}
	want1 := bridgeRoomToken(creator.OwnFingerprint, creator.ExpectedPeerFingerprint, "pair1")
	want2 := bridgeRoomToken(creator.OwnFingerprint, creator.ExpectedPeerFingerprint, "pair2")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				var tok [32]byte
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				if _, err := io.ReadFull(c, tok[:]); err != nil {
					_ = c.Close()
					return
				}
				_ = c.SetReadDeadline(time.Time{})
				switch tok {
				case want1:
					b.pair1 <- c
				case want2:
					b.pair2 <- c
				default:
					b.other <- c
				}
			}()
		}
	}()
	return b
}

// take returns the next leg on ch, or an error when none arrives. It runs on
// the joiner's goroutine, so it reports rather than fails the test itself.
func (b *tokenBridge) take(ch chan net.Conn, what string) (net.Conn, error) {
	select {
	case c := <-ch:
		return c, nil
	case <-time.After(10 * time.Second):
		return nil, fmt.Errorf("no %s leg reached the bridge", what)
	}
}

// requireLegAlive proves a leg carries traffic both ways after the round: what
// one end writes, the other reads. The ends are the SecureConns the handshakes
// installed, so a leg the round closed fails here on its first read.
func requireLegAlive(t *testing.T, a, b net.Conn, label string) {
	t.Helper()
	for _, dir := range []struct {
		w, r net.Conn
		msg  string
	}{{a, b, "ping"}, {b, a, "pong"}} {
		_, err := dir.w.Write([]byte(dir.msg))
		require.NoError(t, err, "%s: write", label)
		buf := make([]byte, len(dir.msg))
		_ = dir.r.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, err = io.ReadFull(dir.r, buf)
		require.NoError(t, err, "%s: read", label)
		require.Equal(t, dir.msg, string(buf), label)
	}
}

// closeSessionConns releases the sockets a round installed. Typed so a nil
// *SecureConn is skipped rather than closed through the interface.
func closeSessionConns(t *testing.T, sessions ...*session.Session) {
	t.Helper()
	t.Cleanup(func() {
		for _, s := range sessions {
			for _, c := range []*session.SecureConn{s.InboundConn(), s.OutboundConn()} {
				if c != nil {
					_ = c.Close()
				}
			}
		}
	})
}

// A joiner that arrives on the bridge leg wins the round with that leg, and the
// round leaves the leg open: it is the session's inbound from then on. This is
// the cold install shape, where the relay probe found the inbound blocked, so
// the round opens no direct window and the bridge leg is the only one.
func TestRendezvousRound_BridgeWinnerKeepsItsLeg(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 5 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, joiner := rendezvousPair(t)
	closeSessionConns(t, creator, joiner)
	bridge := newTokenBridge(t, creator)
	kd, _ := rendezvousKD(t, creator, bridge.addr)
	kd.markInboundBlocked()

	// The joiner's half, on the far end of the bridge: its outbound handshake
	// on the creator's pair1 leg, then its inbound on the creator's pair2 leg.
	joinerDone := make(chan error, 1)
	go func() {
		c1, err := bridge.take(bridge.pair1, "pair1")
		if err != nil {
			joinerDone <- err
			return
		}
		if err := session.PerformOutboundHandshakeOnConn(joiner, c1); err != nil {
			joinerDone <- err
			return
		}
		c2, err := bridge.take(bridge.pair2, "pair2")
		if err != nil {
			joinerDone <- err
			return
		}
		joinerDone <- session.PerformInboundHandshakeWait(joiner, c2, 5*time.Second)
	}()

	done, err := kd.createRendezvousRound(kd.logger, 0)
	require.NoError(t, err)
	require.True(t, done, "the joiner spoke on the bridge leg")
	require.Equal(t, "bridge", kd.ConnectionMode)
	select {
	case err := <-joinerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the joiner's handshakes never finished")
	}

	// pair1 is the leg that won. The round used to close it right here, and the
	// joiner's gRPC client, built on the other end 40 ms later, wrote its
	// preface into a socket the bridge had already dropped.
	requireLegAlive(t, joiner.OutboundConn(), creator.InboundConn(), "pair1, the leg that won")
	requireLegAlive(t, creator.OutboundConn(), joiner.InboundConn(), "pair2")
	select {
	case c := <-bridge.other:
		_ = c.Close()
		t.Fatal("the round dialed a leg with a token neither direction expects")
	default:
	}
}

// The other winner: a joiner that arrives on the listener while the bridge leg
// waits in silence. That leg lost, so the round closes it (the bridge reads
// EOF), and nothing dials the bridge again for a round that is over.
func TestRendezvousRound_DirectWinnerClosesTheBridgeLegForGood(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 5 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	creator, joiner := rendezvousPair(t)
	closeSessionConns(t, creator, joiner)
	joiner.OwnMixedLegs = true      // keeps its direct leg, so it waits for the acknowledgement
	joiner.OwnInboundBlocked = true // the creator skips the dial back and takes pair2
	bridge := newTokenBridge(t, creator)
	kd, ln := rendezvousKD(t, creator, bridge.addr)

	// The joiner dials the listener only once the creator's bridge leg is up
	// and waiting, so the direct win has a silent bridge leg to close.
	silent := make(chan net.Conn, 1)
	joinerDone := make(chan error, 1)
	go func() {
		c1, err := bridge.take(bridge.pair1, "pair1")
		if err != nil {
			joinerDone <- err
			return
		}
		silent <- c1
		if err := session.PerformOutboundHandshake(joiner, ln.Addr().String()); err != nil {
			joinerDone <- err
			return
		}
		c2, err := bridge.take(bridge.pair2, "pair2")
		if err != nil {
			joinerDone <- err
			return
		}
		joinerDone <- session.PerformInboundHandshakeWait(joiner, c2, 5*time.Second)
	}()

	done, err := kd.createRendezvousRound(kd.logger, 0)
	require.NoError(t, err)
	require.True(t, done, "the joiner arrived on the listener")
	require.Equal(t, ModeDirectIn, kd.ConnectionMode)
	select {
	case err := <-joinerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the joiner's handshakes never finished")
	}

	lost := <-silent
	defer lost.Close()
	_ = lost.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = lost.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "the bridge leg that lost is closed, not parked")

	// A round that is won dials no fresh leg. The dial the old code made here
	// followed the close within a millisecond, so this window is ample.
	select {
	case c := <-bridge.pair1:
		_ = c.Close()
		t.Fatal("a fresh bridge leg was dialed for a round that was already won")
	case <-time.After(300 * time.Millisecond):
	}
	requireLegAlive(t, joiner.OutboundConn(), creator.InboundConn(), "the direct leg")
	requireLegAlive(t, creator.OutboundConn(), joiner.InboundConn(), "pair2")
}
