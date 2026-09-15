// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This file drives the creator's real rendezvous round and the real local-mode key
// exchange the way two peers on one LAN run them, and pins what broke on
// 2026-09-15 (make run-alice against make run-bob on one Mac): the creator applied
// the relay's verdict on its public address to a peer on its own subnet and never
// took the joiner's LAN dial, and the bridge fallback underneath hashed the
// literal "TOFU" into the room token, so the two sides waited in different rooms.

package common

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// localModePeer is one side of a local-mode pair: its session and the listener
// its inbound port names, on 127.0.0.1 in the peer port range, as NewKeibiDrop
// opens it.
type localModePeer struct {
	s    *session.Session
	ln   net.Listener
	port int
}

func newLocalModePeer(t *testing.T) *localModePeer {
	t.Helper()
	port := pickFreePortPair(t)
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	s, err := session.InitSession(roundTestLogger(), port+1, port)
	require.NoError(t, err)
	s.ExpectedPeerFingerprint = "TOFU" // what SetPeerDirectAddress leaves for the handshake
	return &localModePeer{s: s, ln: ln, port: port}
}

func (p *localModePeer) addr() string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(p.port))
}

// localModePair returns a creator and a joiner as CreateRoom and JoinRoom leave
// them before the creator's first round: local mode, the peer's LAN address from
// discovery, the keys exchanged over the creator's listener by the real
// plaintext exchange, with the creator running its real accept half.
func localModePair(t *testing.T, bridge string) (kd *KeibiDrop, creator, joiner *localModePeer) {
	t.Helper()
	creator = newLocalModePeer(t)
	joiner = newLocalModePeer(t)
	kd = newBareKD()
	kd.logger = roundTestLogger()
	kd.wallet = &TokenWallet{}
	kd.session = creator.s
	kd.listener = creator.ln
	kd.inboundPort = creator.port
	kd.LocalIPv6IP = "2001:db8::1"
	kd.BridgeAddr = bridge
	kd.ctx = context.Background()
	kd.IsLocalMode = true
	kd.PeerIPv6IP = "127.0.0.1"
	kd.PeerLocalAddrs = []string{"127.0.0.1"}

	joinerDone := make(chan error, 1)
	go func() {
		c, err := session.DialWithStableAddr("tcp", creator.addr(), 15*time.Second, kd.logger)
		if err != nil {
			joinerDone <- err
			return
		}
		defer c.Close()
		joinerDone <- session.ExchangePublicKeysLocal(joiner.s, c, true)
	}()
	require.NoError(t, kd.acceptLocalKeyExchange(kd.logger))
	requireJoined(t, joinerDone)
	require.Equal(t, joiner.port, creator.s.PeerPort, "the creator learned the joiner's inbound port")
	return kd, creator, joiner
}

// joinLAN runs the joiner's LAN path from JoinRoom as written there: dial the
// creator's LAN address, send the handshake, then wait five seconds on its own
// listener for the creator's dial back and answer it.
func joinLAN(joiner *localModePeer, creatorAddr string) error {
	conn, err := net.DialTimeout("tcp", creatorAddr, 2*time.Second)
	if err != nil {
		return fmt.Errorf("LAN dial: %w", err)
	}
	if err := session.PerformOutboundHandshakeOnConn(joiner.s, conn); err != nil {
		conn.Close()
		return fmt.Errorf("LAN handshake: %w", err)
	}
	ln := joiner.ln.(*net.TCPListener)
	_ = ln.SetDeadline(time.Now().Add(5 * time.Second))
	inConn, err := ln.Accept()
	_ = ln.SetDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("LAN inbound accept: %w", err)
	}
	return session.PerformInboundHandshake(joiner.s, inConn)
}

// runRounds is CreateRoom's loop: rounds until one connects, within budget.
func runRounds(kd *KeibiDrop, budget time.Duration) (int, error) {
	deadline := time.Now().Add(budget)
	for round := 0; ; round++ {
		done, err := kd.createRendezvousRound(kd.logger, round)
		if err != nil {
			return round, err
		}
		if done {
			return round, nil
		}
		if !time.Now().Before(deadline) {
			return round, fmt.Errorf("no joiner connected within %s", budget)
		}
	}
}

func requireJoined(t *testing.T, joinerDone <-chan error) {
	t.Helper()
	select {
	case err := <-joinerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the joiner never finished")
	}
}

// requireLANSession proves the round ended as a LAN session with both legs
// carrying traffic: the joiner's dial into the creator and the creator's dial back.
func requireLANSession(t *testing.T, kd *KeibiDrop, joiner *localModePeer) {
	t.Helper()
	require.Equal(t, "lan", kd.ConnectionMode)
	requireLegAlive(t, joiner.s.OutboundConn(), kd.session.InboundConn(), "the joiner's LAN dial")
	requireLegAlive(t, kd.session.OutboundConn(), joiner.s.InboundConn(), "the creator's dial back")
}

// The plaintext key exchange pins the peer's fingerprint on both sides, so the
// bridge room tokens agree before any PQC handshake ran. Until now they were
// computed from the literal "TOFU", and the tokens logged on 2026-09-15 are the
// sha256 of each side's own fingerprint with that word: two rooms.
func TestLocalKeyExchange_PinsThePeerFingerprintForTheBridgeToken(t *testing.T) {
	_, creator, joiner := localModePair(t, "")
	require.Equal(t, joiner.s.OwnFingerprint, creator.s.ExpectedPeerFingerprint)
	require.Equal(t, creator.s.OwnFingerprint, joiner.s.ExpectedPeerFingerprint)
	for _, dir := range []string{"pair1", "pair2"} {
		require.Equal(t,
			bridgeRoomToken(creator.s.OwnFingerprint, creator.s.ExpectedPeerFingerprint, dir),
			bridgeRoomToken(joiner.s.OwnFingerprint, joiner.s.ExpectedPeerFingerprint, dir),
			"%s: both sides must dial the same room", dir)
	}
}

// The creator in local mode takes the joiner's LAN dial although the relay's
// probe found its public address blocked. That verdict is about the internet;
// the joiner is on the same subnet and reached the listener in the same second.
func TestRendezvousRound_LocalModeTakesTheLANDialDespiteTheProbeVerdict(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 2 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	kd, creator, joiner := localModePair(t, silentBridge(t))
	closeSessionConns(t, creator.s, joiner.s)
	kd.markInboundBlocked() // the earlier internet session's probe verdict, still cached
	require.True(t, kd.InboundBlocked())

	joinerDone := make(chan error, 1)
	go func() { joinerDone <- joinLAN(joiner, creator.addr()) }()

	done, err := kd.createRendezvousRound(kd.logger, 0)
	require.NoError(t, err)
	require.True(t, done, "the creator's first round took the LAN dial")
	requireJoined(t, joinerDone)
	requireLANSession(t, kd, joiner)
}

// Whenever the joiner arrives relative to the creator's rounds, the pair ends up
// on the LAN: before the first round (the joiner clicked first, so its dial waits
// in the backlog), during a round, on a round boundary, rounds later, and at
// random offsets.
func TestRendezvousRound_LocalModeJoinerTiming(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 400 * time.Millisecond
	defer func() { bridgeRoundWait = origWait }()

	type timing struct {
		name     string
		joinerAt time.Duration // relative to the first round; negative is before it
	}
	cases := []timing{
		{"joiner first, its dial waits in the backlog", -150 * time.Millisecond},
		{"both at once", 0},
		{"joiner 50 ms in", 50 * time.Millisecond},
		{"joiner mid round", 250 * time.Millisecond},
		{"joiner on the round boundary", bridgeRoundWait},
		{"joiner in the second round", bridgeRoundWait + 200*time.Millisecond},
		{"joiner in the fourth round", 3*bridgeRoundWait + 100*time.Millisecond},
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < 5; i++ {
		d := time.Duration(rng.Int63n(int64(3 * bridgeRoundWait)))
		cases = append(cases, timing{fmt.Sprintf("jitter %d, joiner %s in", i, d.Round(time.Millisecond)), d})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kd, creator, joiner := localModePair(t, silentBridge(t))
			closeSessionConns(t, creator.s, joiner.s)
			kd.markInboundBlocked()

			joinerDone := make(chan error, 1)
			start := func() { joinerDone <- joinLAN(joiner, creator.addr()) }
			if tc.joinerAt < 0 {
				go start()
				time.Sleep(-tc.joinerAt)
			} else {
				time.AfterFunc(tc.joinerAt, start)
			}
			round, err := runRounds(kd, 10*time.Second)
			require.NoError(t, err)
			requireJoined(t, joinerDone)
			requireLANSession(t, kd, joiner)
			t.Logf("joiner at %s connected in round %d", tc.joinerAt, round)
		})
	}
}

// When the LAN dial cannot get through (client isolation on the access point),
// the bridge has to rescue the pair, so both sides must dial the same room. The
// bridge here pairs by the creator's tokens; the joiner computes its own from its
// session, as JoinRoom's fallback does, and they must be the same.
func TestRendezvousRound_LocalModeBridgeFallbackPairsOneRoom(t *testing.T) {
	origWait := bridgeRoundWait
	bridgeRoundWait = 5 * time.Second
	defer func() { bridgeRoundWait = origWait }()

	kd, creator, joiner := localModePair(t, "")
	closeSessionConns(t, creator.s, joiner.s)
	bridge := newTokenBridge(t, creator.s)
	kd.BridgeAddr = bridge.addr
	kd.markInboundBlocked()

	require.Equal(t,
		bridgeRoomToken(creator.s.OwnFingerprint, creator.s.ExpectedPeerFingerprint, "pair1"),
		bridgeRoomToken(joiner.s.OwnFingerprint, joiner.s.ExpectedPeerFingerprint, "pair1"),
		"the joiner dials the room the creator opened")

	joinerDone := make(chan error, 1)
	go func() {
		c1, err := bridge.take(bridge.pair1, "pair1")
		if err != nil {
			joinerDone <- err
			return
		}
		if err := session.PerformOutboundHandshakeOnConn(joiner.s, c1); err != nil {
			joinerDone <- err
			return
		}
		c2, err := bridge.take(bridge.pair2, "pair2")
		if err != nil {
			joinerDone <- err
			return
		}
		joinerDone <- session.PerformInboundHandshakeWait(joiner.s, c2, 5*time.Second)
	}()

	done, err := kd.createRendezvousRound(kd.logger, 0)
	require.NoError(t, err)
	require.True(t, done, "the joiner spoke on the bridge leg")
	require.Equal(t, "bridge", kd.ConnectionMode)
	requireJoined(t, joinerDone)
	requireLegAlive(t, joiner.s.OutboundConn(), creator.s.InboundConn(), "pair1")
	requireLegAlive(t, creator.s.OutboundConn(), joiner.s.InboundConn(), "pair2")
}

// The creator's key exchange accept skips what the backlog holds ahead of the
// joiner: the handshake bytes of a LAN dial an earlier attempt never took (the
// "invalid character '\x00'" that failed the create on 2026-09-15), and a
// connection that never speaks.
func TestAcceptLocalKeyExchange_SkipsWhatTheBacklogHolds(t *testing.T) {
	origWait := localKeyExchangeWait
	localKeyExchangeWait = 300 * time.Millisecond
	defer func() { localKeyExchangeWait = origWait }()

	creator := newLocalModePeer(t)
	joiner := newLocalModePeer(t)
	kd := newBareKD()
	kd.logger = roundTestLogger()
	kd.session = creator.s
	kd.listener = creator.ln
	kd.IsLocalMode = true

	// A handshake dial nobody accepted: a length prefix, part of a payload, closed.
	stale, err := net.Dial("tcp", creator.addr())
	require.NoError(t, err)
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], 512)
	_, err = stale.Write(append(lenBuf[:], []byte(`{"fingerprint":"`)...))
	require.NoError(t, err)
	_ = stale.Close()

	// A connection that holds without speaking.
	silent, err := net.Dial("tcp", creator.addr())
	require.NoError(t, err)
	defer silent.Close()

	joinerDone := make(chan error, 1)
	go func() {
		c, err := net.Dial("tcp", creator.addr())
		if err != nil {
			joinerDone <- err
			return
		}
		defer c.Close()
		joinerDone <- session.ExchangePublicKeysLocal(joiner.s, c, true)
	}()

	began := time.Now()
	require.NoError(t, kd.acceptLocalKeyExchange(kd.logger))
	requireJoined(t, joinerDone)
	require.Equal(t, joiner.s.OwnFingerprint, creator.s.ExpectedPeerFingerprint)
	require.Equal(t, joiner.port, creator.s.PeerPort)
	require.Less(t, time.Since(began), 3*time.Second, "one bounded wait for the silent connection, nothing more")
}

// A closed listener ends the wait with its error rather than looping on it, and a
// nil listener is the bail-out CreateRoom always had.
func TestAcceptLocalKeyExchange_ClosedOrNilListenerReturns(t *testing.T) {
	creator := newLocalModePeer(t)
	kd := newBareKD()
	kd.logger = roundTestLogger()
	kd.session = creator.s
	kd.listener = creator.ln
	_ = creator.ln.Close()
	require.Error(t, kd.acceptLocalKeyExchange(kd.logger))

	kd.listener = nil
	require.ErrorIs(t, kd.acceptLocalKeyExchange(kd.logger), ErrListenerNotOpen)
}
