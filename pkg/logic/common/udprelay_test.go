// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests the UDP relay client against a stand-in relay that mirrors the deployed
// ABOUTME: bridge's wire behaviour (KeibiDrop-DataRelay cmd/bridge/udp.go).

package common

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
	"github.com/stretchr/testify/require"
)

// fakeRelay mirrors the deployed bridge's UDP room behaviour: pair two sources that send the
// same token, ACK both with the bare magic, then forward raw datagrams between them. Kept
// deliberately faithful, so a client that passes here talks to the real relay.
type fakeRelay struct {
	conn *net.UDPConn

	mu    sync.Mutex
	wait  map[string]*fakeWaiter  // token hex -> first peer to register
	peer  map[string]*net.UDPAddr // src -> paired counterpart
	seen  map[string]time.Time    // src -> last datagram, for the idle GC
	pairs int                     // pairings made, so a test can count self-pairs
	drop  func(pkt []byte) bool   // limiter stand-in: report true to swallow a forward
}

// fakeWaiter mirrors the bridge's udpWaiter: an unpaired registration and when it arrived.
type fakeWaiter struct {
	addr    *net.UDPAddr
	created time.Time
}

func startFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv6loopback})
	require.NoError(t, err)
	r := &fakeRelay{
		conn: conn,
		wait: map[string]*fakeWaiter{},
		peer: map[string]*net.UDPAddr{},
		seen: map[string]time.Time{},
	}
	go r.serve()
	t.Cleanup(func() { _ = conn.Close() })
	return r
}

// gc mirrors the bridge's gc: drop waiters past waitTTL and pairs idle past idleTTL.
func (r *fakeRelay) gc(waitTTL, idleTTL time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for tok, w := range r.wait {
		if now.Sub(w.created) > waitTTL {
			delete(r.wait, tok)
		}
	}
	for addr, last := range r.seen {
		if now.Sub(last) > idleTTL {
			if dst, ok := r.peer[addr]; ok {
				delete(r.peer, dst.String())
				delete(r.seen, dst.String())
			}
			delete(r.peer, addr)
			delete(r.seen, addr)
		}
	}
}

// pairCount reports how many pairings the relay has made.
func (r *fakeRelay) pairCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairs
}

func (r *fakeRelay) addr() string { return r.conn.LocalAddr().String() }

func (r *fakeRelay) serve() {
	buf := make([]byte, 65535)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		r.dispatch(buf[:n], src)
	}
}

// dispatch mirrors cmd/bridge/udp.go dispatch + registerLocked, including the two
// behaviours that matter for the room lifecycle: a registration is 32 or 64 bytes after
// the magic (the 64-byte form carries a funded anchor), and pairing is on DISTINCT
// address only, so it will happily pair a client with its own abandoned socket.
func (r *fakeRelay) dispatch(pkt []byte, src *net.UDPAddr) {
	key := src.String()
	rest := 0
	if bytes.HasPrefix(pkt, udpRelayMagic) {
		rest = len(pkt) - len(udpRelayMagic)
	}
	isReg := rest == 32 || rest == 64

	r.mu.Lock()
	if dst, ok := r.peer[key]; ok {
		r.seen[key] = time.Now() // A paired peer's traffic refreshes its idle timer.
		drop := r.drop
		r.mu.Unlock()
		if isReg { // already paired: re-ACK, never forward the magic
			_, _ = r.conn.WriteToUDP(udpRelayMagic, src)
			return
		}
		if drop != nil && drop(pkt) {
			return // limiter stand-in swallowed it
		}
		_, _ = r.conn.WriteToUDP(pkt, dst)
		return
	}
	if !isReg { // unpaired non-registration: drop, QUIC retransmits
		r.mu.Unlock()
		return
	}
	token := hex.EncodeToString(pkt[len(udpRelayMagic) : len(udpRelayMagic)+32])
	if w, ok := r.wait[token]; ok && w.addr.String() != key {
		delete(r.wait, token)
		now := time.Now()
		r.peer[w.addr.String()], r.peer[key] = src, w.addr
		r.seen[w.addr.String()], r.seen[key] = now, now
		r.pairs++
		r.mu.Unlock()
		_, _ = r.conn.WriteToUDP(udpRelayMagic, w.addr)
		_, _ = r.conn.WriteToUDP(udpRelayMagic, src)
		return
	}
	// Same address, or no waiter: park and refresh. This is what makes one socket per
	// room safe, and what makes a socket per attempt dangerous.
	r.wait[token] = &fakeWaiter{addr: src, created: time.Now()}
	r.mu.Unlock()
}

func relayToken(t *testing.T, direction string) []byte {
	t.Helper()
	sum := sha256.Sum256([]byte(direction))
	return sum[:]
}

func regDatagram(t *testing.T, direction string) []byte {
	t.Helper()
	return append(append([]byte(nil), udpRelayMagic...), relayToken(t, direction)...)
}

// registerUDPRelay must block until the counterpart registers the same token, then return a
// deadline-free socket: a leftover read deadline would break QUIC's first read.
func TestUDPRelay_RegisterPairsOnMatchingToken(t *testing.T) {
	relay := startFakeRelay(t)
	relayAddr, err := net.ResolveUDPAddr("udp", relay.addr())
	require.NoError(t, err)

	a, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	defer a.Close()
	b, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	join := testkit.Go(func() error { return registerUDPRelay(ctx, a, relayAddr, regDatagram(t, "quic1")) })
	require.NoError(t, registerUDPRelay(ctx, b, relayAddr, regDatagram(t, "quic1")))
	require.NoError(t, join())

	// Paired: a datagram from one side reaches the other through the relay.
	_, err = a.WriteToUDP([]byte("through the room"), relayAddr)
	require.NoError(t, err)
	buf := make([]byte, 64)
	require.NoError(t, b.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, src, err := b.ReadFromUDP(buf)
	require.NoError(t, err, "the socket must be usable right after registration")
	require.Equal(t, []byte("through the room"), buf[:n])
	require.True(t, sameUDPAddr(src, relayAddr), "the peer appears at the relay address")
}

// A mismatched token must never pair, so a stray peer cannot join someone else's room.
func TestUDPRelay_MismatchedTokenNeverPairs(t *testing.T) {
	relay := startFakeRelay(t)
	relayAddr, err := net.ResolveUDPAddr("udp", relay.addr())
	require.NoError(t, err)

	a, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	defer a.Close()
	b, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = registerUDPRelay(ctx, b, relayAddr, regDatagram(t, "quic2")) }()
	err = registerUDPRelay(ctx, a, relayAddr, regDatagram(t, "quic1"))
	require.Error(t, err, "different tokens are different rooms; registration must time out")
}

// The whole point: QUIC must run over the paired sockets, since neither side can reach the
// other directly. Proves the handover (registered socket -> quic.Transport) end to end.
func TestUDPRelay_QUICRunsOverPairedSockets(t *testing.T) {
	relay := startFakeRelay(t)
	relayAddr, err := net.ResolveUDPAddr("udp", relay.addr())
	require.NoError(t, err)

	server, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)
	client, err := net.ListenUDP("udp", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	join := testkit.Go(func() error { return registerUDPRelay(ctx, server, relayAddr, regDatagram(t, "quic1")) })
	require.NoError(t, registerUDPRelay(ctx, client, relayAddr, regDatagram(t, "quic1")))
	require.NoError(t, join())

	ln, err := transport.ListenOnConn(server)
	require.NoError(t, err)
	defer ln.Close()

	const msg = "relayed quic payload"
	// NOT testkit.Go: the goroutine's cleanup defer blocks on done, which this
	// function only closes after reading the result. A return statement waits
	// for that defer before testkit.Go's channel ever receives it: deadlock.
	// The plain buffered send below does not wait for that defer, so it stays safe.
	accepted := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, aErr := ln.Accept()
		if aErr != nil {
			accepted <- aErr
			return
		}
		// Held open until the test ends: quicConn.Close resets the stream, which would
		// discard the reply before the client reads it.
		defer func() { <-done; _ = c.Close() }()
		got := make([]byte, len(msg))
		if _, rErr := io.ReadFull(c, got); rErr != nil {
			accepted <- rErr
			return
		}
		if string(got) != msg {
			accepted <- io.ErrUnexpectedEOF
			return
		}
		_, wErr := c.Write([]byte("ack"))
		accepted <- wErr
	}()

	conn, err := transport.DialOnConn(ctx, client, relayAddr)
	require.NoError(t, err, "QUIC must complete its handshake through the relay")
	defer conn.Close()

	_, err = conn.Write([]byte(msg))
	require.NoError(t, err)
	back := make([]byte, 3)
	_, err = io.ReadFull(conn, back)
	require.NoError(t, err)
	require.Equal(t, "ack", string(back))
	require.NoError(t, <-accepted)
}

// The two peers must derive opposite room pairs, or a dialer never meets a listener.
func TestUDPRelay_RoomsAreOppositeAcrossPeers(t *testing.T) {
	lo := &session.Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb"}
	hi := &session.Session{OwnFingerprint: "bbb", ExpectedPeerFingerprint: "aaa"}

	loDial, loListen := relayQUICRooms(lo)
	hiDial, hiListen := relayQUICRooms(hi)

	require.Equal(t, loDial, hiListen, "one peer's dial room is the other's listen room")
	require.Equal(t, hiDial, loListen)
	require.NotEqual(t, loDial, loListen, "a peer must not dial and listen in the same room")
}

// Both halves of a peer must register concurrently. A peer's listen room is paired by the
// PEER's dial room, so if both peers register listen-then-dial neither ever reaches its dial
// half and all four rooms stall. Locks the ordering startQUICControlRelay depends on.
func TestUDPRelay_SerializedHalvesDeadlockConcurrentHalvesPair(t *testing.T) {
	relay := startFakeRelay(t)
	relayAddr, err := net.ResolveUDPAddr("udp", relay.addr())
	require.NoError(t, err)

	reg := func(ctx context.Context, room string) error {
		udp, uErr := net.ListenUDP("udp", nil)
		require.NoError(t, uErr)
		t.Cleanup(func() { _ = udp.Close() })
		return registerUDPRelay(ctx, udp, relayAddr, regDatagram(t, room))
	}
	peers := [][2]string{{"quic2", "quic1"}, {"quic1", "quic2"}} // {listen, dial} per peer

	// Serialized listen-then-dial on both peers: nothing pairs.
	sctx, scancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer scancel()
	stalled := make(chan error, len(peers))
	var swg sync.WaitGroup
	for _, p := range peers {
		swg.Add(1)
		go func(listen string) {
			defer swg.Done()
			stalled <- reg(sctx, listen) // never returns in time, so the dial half never runs
		}(p[0])
	}
	swg.Wait()
	close(stalled)
	for e := range stalled {
		require.Error(t, e, "a listen room cannot pair while both peers withhold their dial room")
	}

	// Concurrent halves against a fresh relay: all four rooms pair.
	relay2 := startFakeRelay(t)
	relayAddr, err = net.ResolveUDPAddr("udp", relay2.addr())
	require.NoError(t, err)
	cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer ccancel()
	paired := make(chan error, 2*len(peers))
	var cwg sync.WaitGroup
	for _, p := range peers {
		for _, room := range p {
			cwg.Add(1)
			go func(room string) { defer cwg.Done(); paired <- reg(cctx, room) }(room)
		}
	}
	cwg.Wait()
	close(paired)
	for e := range paired {
		require.NoError(t, e, "concurrent halves pair every room")
	}
}
