// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests for the inbound scanner filter: probes, junk and silence never reach Accept, a peer's
// ABOUTME: first bytes come back intact, early dials wait for the window, deadlines and Close act as on TCP.

package common

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// handshakeHello is how a real PQC handshake starts: a 4 KiB big-endian length, then the JSON's brace.
var handshakeHello = []byte{0, 0, 0x10, 0, '{'}

func newTestInbound(t *testing.T, sniff, parkAge time.Duration) (*inboundListener, string) {
	t.Helper()
	oldSniff, oldAge := sniffWait, maxParkAge
	sniffWait, maxParkAge = sniff, parkAge
	ln, err := listenInbound("tcp", "127.0.0.1:0")
	sniffWait, maxParkAge = oldSniff, oldAge
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	return ln.(*inboundListener), ln.Addr().String()
}

func dialAndWrite(t *testing.T, addr string, b []byte) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	if len(b) > 0 {
		_, err = c.Write(b)
		require.NoError(t, err)
	}
	return c
}

func acceptWithin(l *inboundListener, d time.Duration) (net.Conn, error) {
	_ = l.SetDeadline(time.Now().Add(d))
	defer func() { _ = l.SetDeadline(time.Time{}) }()
	return l.Accept()
}

// closedByPeer reports whether the listener side closed c: a read ends with EOF or a reset, not a timeout.
func closedByPeer(c net.Conn, within time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(within))
	_, err := io.ReadAll(c)
	var ne net.Error
	return !errors.As(err, &ne) || !ne.Timeout()
}

func waitParked(t *testing.T, l *inboundListener, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return len(l.parked) == n
	}, 3*time.Second, 5*time.Millisecond, "expected %d parked connections", n)
}

func TestPeerShaped(t *testing.T) {
	for name, tc := range map[string]struct {
		head string
		ok   bool
	}{
		"pqc handshake":         {"\x00\x00\x10\x00{", true},
		"pqc at the 64 KiB cap": {"\x00\x01\x00\x00{", true},
		"pqc over the cap":      {"\x00\x01\x00\x01{", false},
		"zero length":           {"\x00\x00\x00\x00{", false},
		"length without brace":  {"\x00\x00\x10\x00[", false},
		"lan key exchange":      {"{\"pub", true},
		"brace without quote":   {"{ \"pu", false},
		"http":                  {"GET /", false},
		"tls client hello":      {"\x16\x03\x01\x02\x00", false},
		"ssh banner":            {"SSH-2", false},
		"smb over tcp":          {"\x00\x00\x00\x85\xff", false},
		"rdp tpkt":              {"\x03\x00\x00\x13\x0e", false},
	} {
		var b [5]byte
		copy(b[:], tc.head)
		require.Equal(t, tc.ok, peerShaped(b), name)
	}
}

// The bug this filter fixes: before it, whatever reached the port first sat at the head of the kernel
// queue, and the LAN path's single accept got a scanner instead of the peer.
func TestInbound_ScannersNeverReachAcceptAndThePeerDoes(t *testing.T) {
	l, addr := newTestInbound(t, 200*time.Millisecond, time.Minute)

	junk := []net.Conn{
		dialAndWrite(t, addr, []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")),
		dialAndWrite(t, addr, []byte{0x16, 0x03, 0x01, 0x02, 0x00, 0x01, 0x00, 0x01, 0xfc}),
		dialAndWrite(t, addr, []byte("SSH-2.0-OpenSSH_9.6\r\n")),
		dialAndWrite(t, addr, []byte{0, 0, 0, 0x85, 0xff, 'S', 'M', 'B'}),
		dialAndWrite(t, addr, nil), // silent
	}
	probe, err := net.Dial("tcp", addr) // the relay's reachability probe: connect, close, no bytes
	require.NoError(t, err)
	require.NoError(t, probe.Close())

	peer := dialAndWrite(t, addr, append(append([]byte(nil), handshakeHello...), []byte(`"public_keys":{}}`)...))

	c, err := acceptWithin(l, 2*time.Second)
	require.NoError(t, err, "the peer must get through the junk queued before it")
	require.Equal(t, peer.LocalAddr().String(), c.RemoteAddr().String())
	_ = c.Close()

	_, err = acceptWithin(l, 500*time.Millisecond)
	require.True(t, os.IsTimeout(err), "nothing but the peer may reach Accept, got %v", err)
	for i, j := range junk {
		require.True(t, closedByPeer(j, 2*time.Second), "junk connection %d must be closed by the listener", i)
	}
}

func TestInbound_PeerBytesComeBackIntact(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, time.Minute)
	for name, payload := range map[string][]byte{
		"pqc handshake":    append(append([]byte(nil), handshakeHello...), []byte(`"fingerprint":"x","public_keys":{}}`)...),
		"lan key exchange": []byte(`{"public_keys":{"x25519":"AA"},"port":26431}` + "\n"),
	} {
		dialAndWrite(t, addr, payload)
		c, err := acceptWithin(l, 2*time.Second)
		require.NoError(t, err, name)
		got := make([]byte, len(payload))
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = io.ReadFull(c, got)
		require.NoError(t, err, name)
		require.Equal(t, payload, got, name)
		_ = c.Close()
	}
}

// A peer may dial before this side opens its window; the kernel queue used to hold it. The park does.
func TestInbound_EarlyDialWaitsForTheWindow(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, time.Minute)
	peer := dialAndWrite(t, addr, handshakeHello)
	waitParked(t, l, 1)

	c, err := acceptWithin(l, time.Second)
	require.NoError(t, err)
	require.Equal(t, peer.LocalAddr().String(), c.RemoteAddr().String())
	_ = c.Close()
}

func TestInbound_ParkIsBoundedOldestFirst(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, time.Minute)
	var peers []net.Conn
	for i := 0; i < maxParked+2; i++ {
		peers = append(peers, dialAndWrite(t, addr, handshakeHello))
		waitParked(t, l, min(i+1, maxParked))
	}
	require.True(t, closedByPeer(peers[0], 2*time.Second), "the oldest parked connection is closed first")
	require.True(t, closedByPeer(peers[1], 2*time.Second))
	for _, want := range peers[2:] {
		c, err := acceptWithin(l, time.Second)
		require.NoError(t, err)
		require.Equal(t, want.LocalAddr().String(), c.RemoteAddr().String(), "parked connections leave in arrival order")
		_ = c.Close()
	}
}

func TestInbound_StaleParkedConnectionIsDropped(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, 200*time.Millisecond)
	peer := dialAndWrite(t, addr, handshakeHello)
	waitParked(t, l, 1)
	time.Sleep(300 * time.Millisecond)

	_, err := acceptWithin(l, 200*time.Millisecond)
	require.True(t, os.IsTimeout(err), "a connection parked past its age must not be handed out, got %v", err)
	require.True(t, closedByPeer(peer, 2*time.Second))
}

// Accept's deadline and Close behave as on *net.TCPListener, which every accept site relies on.
func TestInbound_DeadlineAndClose(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, time.Minute)

	_, err := acceptWithin(l, 100*time.Millisecond)
	require.True(t, os.IsTimeout(err), "got %v", err)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)

	// directAcceptor.stop ends a waiting Accept by moving the deadline to now.
	done := make(chan error, 1)
	go func() { _, err := l.Accept(); done <- err }()
	time.Sleep(50 * time.Millisecond)
	_ = l.SetDeadline(time.Now())
	select {
	case err := <-done:
		require.True(t, os.IsTimeout(err), "got %v", err)
	case <-time.After(time.Second):
		t.Fatal("a deadline in the past must end a waiting Accept")
	}
	_ = l.SetDeadline(time.Time{})

	parked := dialAndWrite(t, addr, handshakeHello)
	waitParked(t, l, 1)
	go func() { time.Sleep(50 * time.Millisecond); _ = l.Close() }()
	c, err := l.Accept()
	if err == nil { // the parked one may still win the race with Close
		_ = c.Close()
		c, err = l.Accept()
	}
	require.Nil(t, c)
	require.ErrorIs(t, err, net.ErrClosed)
	require.True(t, closedByPeer(parked, 2*time.Second), "Close closes what is parked")

	_, err = net.DialTimeout("tcp", addr, 500*time.Millisecond)
	require.Error(t, err, "a closed listener refuses dials")
}

// directAcceptor.stop ends a round by moving the deadline to now. The next round's peer, parked in the
// meantime, must stay for the next Accept instead of going to the finished round.
func TestInbound_PassedDeadlineLeavesParkedForTheNextAccept(t *testing.T) {
	l, addr := newTestInbound(t, time.Second, time.Minute)
	peer := dialAndWrite(t, addr, handshakeHello)
	waitParked(t, l, 1)

	_ = l.SetDeadline(time.Now())
	_, err := l.Accept()
	require.True(t, os.IsTimeout(err), "got %v", err)

	c, err := acceptWithin(l, time.Second)
	require.NoError(t, err)
	require.Equal(t, peer.LocalAddr().String(), c.RemoteAddr().String())
	_ = c.Close()
}

// The creator's rendezvous round accepts through startDirectAcceptor.
func TestInbound_DirectAcceptorGetsThePeerAndStops(t *testing.T) {
	l, addr := newTestInbound(t, 200*time.Millisecond, time.Minute)
	arrivals := make(chan roundArrival, 8)
	acc := startDirectAcceptor(l, arrivals, 5*time.Second)

	dialAndWrite(t, addr, []byte("GET / HTTP/1.0\r\n\r\n"))
	peer := dialAndWrite(t, addr, handshakeHello)
	select {
	case a := <-arrivals:
		require.NoError(t, a.err)
		require.Equal(t, peer.LocalAddr().String(), a.conn.RemoteAddr().String())
		_ = a.conn.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("the peer never reached the round")
	}

	stopped := make(chan struct{})
	go func() { acc.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop must end the accept loop")
	}
}
