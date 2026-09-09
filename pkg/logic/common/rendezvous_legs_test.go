// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

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

// The peeked byte is the first byte the next reader gets, so a handshake that runs
// after the peek sees the whole message.
func TestPeekConn_ReplaysTheFirstByte(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = b.Write([]byte("hello")) }()
	pc := newPeekConn(a)
	require.NoError(t, pc.peek(time.Second))
	buf := make([]byte, 5)
	_, err := io.ReadFull(pc, buf)
	require.NoError(t, err)
	require.Equal(t, "hello", string(buf))
}

// A silent leg times out (the round parks it); a closed leg is not a timeout (the
// round dials a fresh one).
func TestPeekConn_SilentIsTimeoutClosedIsNot(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	pc := newPeekConn(c)
	err = pc.peek(50 * time.Millisecond)
	require.Error(t, err)
	require.True(t, isTimeout(err), "a silent leg is a timeout: %v", err)

	a, b := net.Pipe()
	_ = b.Close()
	err = newPeekConn(a).peek(time.Second)
	require.Error(t, err)
	require.False(t, isTimeout(err), "a closed leg is not a timeout: %v", err)
	require.True(t, isTimeout(os.ErrDeadlineExceeded))
	require.False(t, isTimeout(errors.New("refused")))
}

// stop ends the accept loop and leaves the listener open and deadline-free, so the
// next accept (a reconnect) is not stolen by a loop still inside Accept and does not
// inherit a dead deadline.
func TestDirectAcceptor_StopLeavesAPlainListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	arrivals := make(chan roundArrival, 8)
	acc := startDirectAcceptor(ln.(*net.TCPListener), arrivals, 5*time.Second)

	c, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c.Close()
	a := <-arrivals
	require.NoError(t, a.err)
	require.Equal(t, "direct", a.via)
	_ = a.conn.Close()

	acc.stop()
	acc.stop() // idempotent
	select {
	case <-acc.done:
	default:
		t.Fatal("the accept loop is still running after stop")
	}

	// The listener accepts again for whoever asks next, with no deadline.
	c2, err := net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	defer c2.Close()
	_ = ln.(*net.TCPListener).SetDeadline(time.Now().Add(time.Second))
	got, err := ln.Accept()
	require.NoError(t, err, "a plain Accept after stop must get the connection")
	_ = got.Close()
}

// The window closing with nobody in it reports a timeout on the direct leg.
func TestDirectAcceptor_WindowEndsWithATimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	arrivals := make(chan roundArrival, 8)
	acc := startDirectAcceptor(ln.(*net.TCPListener), arrivals, 50*time.Millisecond)
	a := <-arrivals
	require.Error(t, a.err)
	require.True(t, isTimeout(a.err))
	acc.stop()
}

func TestBridgeLegHolder_WinnerClosesAndRefusesLater(t *testing.T) {
	h := &bridgeLegHolder{}
	a, b := net.Pipe()
	defer b.Close()
	require.True(t, h.set(a))
	h.closeForWinner()
	_, err := a.Write([]byte{1})
	require.Error(t, err, "the held leg is closed by the winner")
	c, d := net.Pipe()
	defer d.Close()
	require.False(t, h.set(c), "a leg dialed after the win is refused")
	_ = c.Close()
}
