// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: The creator's rendezvous legs: the direct listener and the bridge leg are
// ABOUTME: open at the same time, and the first joiner that speaks wins the round.

package common

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"
)

// roundArrival is one event on one of the round's legs: a peer to handshake with
// (conn set, err nil), or the leg ending without one (err set; conn set when the
// leg is still open and can be parked).
type roundArrival struct {
	conn net.Conn
	via  string // "direct" or "bridge"
	err  error
}

// peekConn holds back the first byte a leg delivers, so the round can learn that a
// joiner is speaking on it without consuming any of the handshake. peek runs on the
// leg's goroutine, Read on the round's, ordered by the arrival channel.
type peekConn struct {
	net.Conn
	first []byte
}

func newPeekConn(c net.Conn) *peekConn { return &peekConn{Conn: c} }

// peek waits up to d for one byte and keeps it for the next Read.
func (p *peekConn) peek(d time.Duration) error {
	_ = p.SetReadDeadline(time.Now().Add(d))
	var b [1]byte
	n, err := p.Conn.Read(b[:])
	_ = p.SetReadDeadline(time.Time{})
	if n == 1 {
		p.first = b[:1]
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (p *peekConn) Read(b []byte) (int, error) {
	if len(p.first) > 0 && len(b) > 0 {
		n := copy(b, p.first)
		p.first = p.first[n:]
		return n, nil
	}
	return p.Conn.Read(b)
}

// isTimeout reports a deadline, as opposed to a leg that was closed.
func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// directAcceptor accepts on the listener for the round and hands every connection
// to the round through arrivals. It exits when the listener's deadline passes or
// stop closes it.
type directAcceptor struct {
	ln       *net.TCPListener
	arrivals chan<- roundArrival
	quit     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func startDirectAcceptor(ln *net.TCPListener, arrivals chan<- roundArrival, window time.Duration) *directAcceptor {
	a := &directAcceptor{ln: ln, arrivals: arrivals, quit: make(chan struct{}), done: make(chan struct{})}
	_ = ln.SetDeadline(time.Now().Add(window))
	go func() {
		defer close(a.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				a.arrivals <- roundArrival{via: "direct", err: err}
				return
			}
			select {
			case a.arrivals <- roundArrival{conn: c, via: "direct"}:
			case <-a.quit:
				_ = c.Close()
				return
			}
		}
	}()
	return a
}

// stop ends the accept loop and clears the listener's deadline, so the listener
// is a plain open listener again for the session (the reconnect manager accepts
// on it). It waits for the loop to exit: a loop still inside Accept would take
// the next connection away from whoever accepts next.
func (a *directAcceptor) stop() {
	a.once.Do(func() {
		close(a.quit)
		_ = a.ln.SetDeadline(time.Now())
		<-a.done
		_ = a.ln.SetDeadline(time.Time{})
	})
}

// bridgeLegHolder is the round's current bridge leg, shared between the leg's
// goroutine (which may replace a closed room with a fresh dial) and the round
// (which closes it once another leg won).
type bridgeLegHolder struct {
	mu   sync.Mutex
	conn net.Conn
	won  bool
}

func (h *bridgeLegHolder) set(c net.Conn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.won {
		return false
	}
	h.conn = c
	return true
}

// closeForWinner closes the current leg and refuses any later one.
func (h *bridgeLegHolder) closeForWinner() {
	h.mu.Lock()
	h.won = true
	c := h.conn
	h.conn = nil
	h.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// watchBridgeLeg waits for a joiner to speak on the bridge leg: the leg parked by
// the previous round first, then a fresh pair1 dial when the bridge has expired
// the parked room. One arrival is sent: a speaking leg, a silent leg still open
// (a timeout; the round parks it), or a failure.
func (kd *KeibiDrop) watchBridgeLeg(logger *slog.Logger, holder *bridgeLegHolder, parked net.Conn, window time.Duration, arrivals chan<- roundArrival) {
	deadline := time.Now().Add(window)
	leg := parked
	for attempt := 0; attempt < 2; attempt++ {
		if leg == nil {
			c, err := kd.dialBridgeDir("pair1", logger)
			if err != nil {
				arrivals <- roundArrival{via: "bridge", err: err}
				return
			}
			leg = c
		}
		if !holder.set(leg) {
			_ = leg.Close() // Another leg won while we dialed.
			arrivals <- roundArrival{via: "bridge", err: errors.New("round already won")}
			return
		}
		pc := newPeekConn(leg)
		err := pc.peek(time.Until(deadline))
		if err == nil {
			arrivals <- roundArrival{conn: pc, via: "bridge"}
			return
		}
		if isTimeout(err) {
			arrivals <- roundArrival{conn: pc, via: "bridge", err: err}
			return
		}
		// The bridge closed the room (it expires unpaired rooms) or the winner
		// closed us. One fresh leg, then give the round up.
		_ = leg.Close()
		leg = nil
		if attempt == 0 {
			logger.Info("Bridge leg closed before a joiner spoke, dialing a fresh one", "error", err)
		}
	}
	arrivals <- roundArrival{via: "bridge", err: errors.New("bridge leg closed twice this round")}
}
