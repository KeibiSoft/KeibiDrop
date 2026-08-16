// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Two-peer tests for the QUIC lane over relay rooms. They assert BOTH sides agree
// ABOUTME: the lane is up, which is the check the 2026-08-16 WAN failure slipped through.

package common

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// lanePeer is one side of a bridge-mode session pointed at a stand-in relay.
type lanePeer struct {
	kd     *KeibiDrop
	cancel context.CancelFunc
}

// newLanePeer builds a KeibiDrop in bridge mode whose UDP rooms live on relayAddr.
func newLanePeer(t *testing.T, s *session.Session, relayAddr string) *lanePeer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	kd := &KeibiDrop{
		logger:         testkit.DiscardLogger(),
		session:        s,
		ctx:            ctx,
		ConnectionMode: "bridge",
		BridgeAddr:     relayAddr,
	}
	p := &lanePeer{kd: kd, cancel: cancel}
	t.Cleanup(func() {
		cancel()
		kd.StopQUICControlChannel()
	})
	return p
}

// inboundAccepted reports this peer's accepted inbound control conns.
func (p *lanePeer) inboundAccepted() uint64 { return p.kd.QUICInboundAccepted() }

// waitLaneBothWays waits until both peers have a verified outbound lane AND both have
// accepted an inbound conn. A one-directional lane fails this and used to pass.
func waitLaneBothWays(t *testing.T, a, b *lanePeer, within time.Duration) error {
	t.Helper()
	return testkit.Poll(within, 100*time.Millisecond, func() bool {
		return a.kd.QUICControlConnected() && b.kd.QUICControlConnected() &&
			a.inboundAccepted() > 0 && b.inboundAccepted() > 0
	}, "both peers to agree the QUIC lane is up in both directions")
}

// The lane-truth test. Two peers over relay rooms must BOTH report the lane up and BOTH
// have accepted an inbound conn.
//
// On 2026-08-16 the creator logged "QUIC control channel connected" while the joiner
// stayed on TCP, because "connected" only meant the relay room had accepted a
// registration. Nothing asserted the two sides agreed.
func TestQUICLane_BothPeersAgreeOverRelayRooms(t *testing.T) {
	relay := startFakeRelay(t)
	alice, bob := handshakenSessionPair(t)

	a := newLanePeer(t, alice, relay.addr())
	b := newLanePeer(t, bob, relay.addr())

	a.kd.StartQUICControlChannel()
	b.kd.StartQUICControlChannel()

	require.NoError(t, waitLaneBothWays(t, a, b, 45*time.Second))
	require.Positive(t, relay.pairCount(), "the rooms must have paired at the relay")
}

// E1b, the joiner-late case. One peer starts 12 seconds after the other, which is what a
// human-paced connect looks like. The retry loops must converge, and the client must never
// pair with its own abandoned socket on the way.
//
// Before the fix the late start produced Bob's exact alternation: attempt 1 pairs with the
// previous attempt's corpse and dies at the QUIC handshake, attempt 2 finds no waiter and
// dies at registration, forever.
func TestQUICLane_ConvergesWhenOnePeerStartsLate(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~30s: a real late start plus a retry cycle")
	}
	relay := startFakeRelay(t)
	alice, bob := handshakenSessionPair(t)

	a := newLanePeer(t, alice, relay.addr())
	b := newLanePeer(t, bob, relay.addr())

	a.kd.StartQUICControlChannel()
	time.Sleep(12 * time.Second) // the peer pastes the code and presses connect
	b.kd.StartQUICControlChannel()

	require.NoError(t, waitLaneBothWays(t, a, b, 60*time.Second))

	// Four sockets, two rooms, so a healthy convergence pairs a handful of times. A
	// self-pair spiral is unbounded: the bridge journal for 2026-08-16 holds 39 pairings
	// on two rooms in ten minutes, against 6 real ones.
	require.LessOrEqual(t, relay.pairCount(), 8,
		"pairings should be bounded; an unbounded count means each attempt rebinds and self-pairs")
}

// The socket-per-room rule, stated directly against the relay's own pairing logic.
//
// A client that binds a NEW socket per attempt hands the relay two distinct addresses for
// one token, and the relay pairs them: the client is now talking to its own corpse. One
// socket per room makes that impossible, because a same-address re-registration only
// refreshes the waiter.
func TestRoomSocket_ReusesOneSocketAcrossFailedRegistrations(t *testing.T) {
	relay := startFakeRelay(t)
	s := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb"}
	kd := &KeibiDrop{logger: testkit.DiscardLogger(), ConnectionMode: "bridge", BridgeAddr: relay.addr()}

	rs := kd.newRoomSocket("quic1")
	defer rs.close()

	ports := map[int]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
		_, _, err := rs.pair(ctx, s, testkit.DiscardLogger())
		cancel()
		require.Error(t, err, "no counterpart registered, so the room cannot pair")
		require.NotNil(t, rs.udp, "the socket must survive a registration timeout")
		la, ok := rs.udp.LocalAddr().(*net.UDPAddr)
		require.True(t, ok)
		ports[la.Port] = true
	}
	require.Len(t, ports, 1, "every attempt must reuse one socket, or the relay pairs us with ourselves")
	require.Zero(t, relay.pairCount(), "one peer alone must never produce a pairing")
}

// The escape rule, and its guard. A socket whose room PAIRED and whose handshake then
// failed must be dropped, because the relay keeps a paired source paired and only re-acks
// it. A socket that never paired must be KEPT, because rebinding would hand the relay a
// second address of ours for one token, which is the self-pair itself.
func TestRoomSocket_PoisonOnlyRebindsAPairedSocket(t *testing.T) {
	relay := startFakeRelay(t)
	s := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb"}
	kd := &KeibiDrop{logger: testkit.DiscardLogger(), ConnectionMode: "bridge", BridgeAddr: relay.addr()}

	rs := kd.newRoomSocket("quic1")
	defer rs.close()

	// Unpaired: poison must be a no-op, or the rebind pairs with our own leftover waiter.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	_, _, err := rs.pair(ctx, s, testkit.DiscardLogger())
	cancel()
	require.Error(t, err)
	unpaired, ok := rs.udp.LocalAddr().(*net.UDPAddr)
	require.True(t, ok)

	rs.poison()
	require.NotNil(t, rs.udp, "an unpaired socket must survive poison")
	kept, ok := rs.udp.LocalAddr().(*net.UDPAddr)
	require.True(t, ok)
	require.Equal(t, unpaired.Port, kept.Port)
	require.Zero(t, relay.pairCount(), "one peer alone must never produce a pairing")

	// Paired: a counterpart registers, the room pairs, and now poison must rebind.
	counterpart, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	defer counterpart.Close()
	relayAddr, err := net.ResolveUDPAddr("udp", relay.addr())
	require.NoError(t, err)
	tok := bridgeRoomToken(s.OwnFingerprint, s.ExpectedPeerFingerprint, "quic1")
	reg := append(append([]byte(nil), udpRelayMagic...), tok[:]...)
	join := testkit.Go(func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return registerUDPRelay(ctx, counterpart, relayAddr, reg)
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	_, _, err = rs.pair(ctx2, s, testkit.DiscardLogger())
	cancel2()
	require.NoError(t, err, "the room must pair once a counterpart registers")
	require.NoError(t, join())

	rs.poison()
	require.Nil(t, rs.udp, "a paired socket must be dropped so the next attempt rebinds")
}

// C4. A stale maintainer from a previous generation can still hold the single-flight flag
// while it sleeps out its re-probe. Arming must wait it out rather than give up, or the
// new generation runs with no dial half at all.
func TestEnsureQUICMaintainer_ArmsAfterAStaleMaintainerReleases(t *testing.T) {
	relay := startFakeRelay(t)
	s := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kd := &KeibiDrop{
		logger:         testkit.DiscardLogger(),
		session:        s,
		ctx:            ctx,
		ConnectionMode: "bridge",
		BridgeAddr:     relay.addr(),
	}

	// A previous generation's maintainer is parked and still holds the flag.
	require.True(t, kd.quicRedialing.CompareAndSwap(false, true))
	kd.mu.Lock()
	kd.quicPeerAddr = relayAddrPrefix + "quic1"
	kd.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		kd.ensureQUICMaintainer(ctx, 7, relayAddrPrefix+"quic1")
	}()

	// While the flag is held, arming must keep waiting rather than return.
	select {
	case <-done:
		t.Fatal("arming gave up while the stale maintainer still held the flag")
	case <-time.After(2 * quicArmRetryInterval):
	}

	// The stale maintainer exits and releases. Arming must then win the flag and run.
	kd.quicRedialing.Store(false)
	require.NoError(t, testkit.Poll(10*time.Second, 50*time.Millisecond, func() bool {
		return kd.quicRedialing.Load()
	}, "arming to take the single-flight flag once the stale maintainer released it"))

	cancel()
	<-done
}
