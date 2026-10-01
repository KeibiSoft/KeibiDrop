// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: kd's inbound listener accepts all the time and keeps only connections that start like a peer.
// ABOUTME: Scanners and reachability probes are closed at once instead of filling the kernel accept queue.

package common

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// The listener is open from daemon start, but Accept runs only inside connect windows. Before this
// filter, every scanner connection waited in the kernel queue until a window took it: one stale entry
// failed the LAN accept, a silent one cost 3 s of a direct window, and a full queue dropped real dials
// (Timisoara, 2026-10-01: about 70 CLOSE-WAIT entries on :26801 from one scanner range).

// sniffWait bounds the wait for a new connection's first bytes. A peer writes as soon as it connects; this
// is the longest first-read wait any accept site had before (the local key exchange's 10 s), so no peer
// gets less time than it did. Only silent sockets wait it out. A variable so tests can shrink it.
var sniffWait = 10 * time.Second

// maxParkAge drops a peer-shaped connection that no window took in time. A variable so tests can shrink it.
var maxParkAge = 60 * time.Second

const (
	// maxSniffing caps connections read at once. Past it the accept loop waits, and new connections
	// wait in the kernel queue as before.
	maxSniffing = 64
	// maxParked caps peer-shaped connections that wait for the next Accept; the oldest is closed first.
	maxParked = 8
	// maxHandshakeLen is the inbound handshake's own limit (session.PerformInboundHandshakeWait).
	maxHandshakeLen = 64 << 10
)

// deadlineListener is a listener whose Accept can be bounded in time: *net.TCPListener and inboundListener.
type deadlineListener interface {
	net.Listener
	SetDeadline(t time.Time) error
}

// inboundListener drains a TCP listener and hands Accept only the connections whose first bytes can start
// a peer exchange. Accept and SetDeadline behave as on *net.TCPListener.
type inboundListener struct {
	ln        *net.TCPListener
	sniffing  chan struct{}
	sniffWait time.Duration // sniffWait and maxParkAge as they were at listen time
	parkAge   time.Duration
	closing   chan struct{} // closed by Close: the accept loop stops waiting for a sniff slot
	drained   chan struct{} // closed when the accept loop has returned

	mu       sync.Mutex
	parked   []parkedConn
	deadline time.Time
	changed  chan struct{} // closed and replaced when parked, deadline or closed changes
	stopping bool          // Close has begun: new arrivals are closed, not parked
	closed   bool          // the socket is closed: Accept returns net.ErrClosed
}

type parkedConn struct {
	c  net.Conn
	at time.Time
}

// listenInbound opens kd's inbound listener with the scanner filter in front of it.
func listenInbound(network, addr string) (net.Listener, error) {
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	l := &inboundListener{ln: ln.(*net.TCPListener), sniffing: make(chan struct{}, maxSniffing), sniffWait: sniffWait, parkAge: maxParkAge,
		closing: make(chan struct{}), drained: make(chan struct{}), changed: make(chan struct{})}
	go l.drain()
	return l, nil
}

func (l *inboundListener) Addr() net.Addr { return l.ln.Addr() }

// Accept returns the oldest parked connection, or waits for one until the deadline or Close.
func (l *inboundListener) Accept() (net.Conn, error) {
	for {
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			return nil, l.opError(net.ErrClosed)
		}
		// A passed deadline wins over a parked connection, as on *net.TCPListener: directAcceptor.stop
		// moves the deadline to now so a finished round does not take the next round's peer.
		if !l.deadline.IsZero() && !time.Now().Before(l.deadline) {
			l.mu.Unlock()
			return nil, l.opError(os.ErrDeadlineExceeded)
		}
		var stale []net.Conn
		var got net.Conn
		for got == nil && len(l.parked) > 0 {
			p := l.parked[0]
			l.parked = l.parked[1:]
			if time.Since(p.at) > l.parkAge {
				stale = append(stale, p.c)
				continue
			}
			got = p.c
		}
		deadline, changed := l.deadline, l.changed
		l.mu.Unlock()
		for _, c := range stale {
			_ = c.Close()
		}
		if got != nil {
			return got, nil
		}
		if deadline.IsZero() {
			<-changed
			continue
		}
		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, l.opError(os.ErrDeadlineExceeded)
		}
		t := time.NewTimer(wait)
		select {
		case <-changed:
		case <-t.C:
		}
		t.Stop()
	}
}

// SetDeadline bounds Accept; the zero time removes the bound. A deadline in the past ends a waiting Accept.
func (l *inboundListener) SetDeadline(t time.Time) error {
	l.mu.Lock()
	l.deadline = t
	l.notifyLocked()
	l.mu.Unlock()
	return nil
}

// Close stops the listener and closes every parked connection. It returns once the accept loop has left
// ln.Accept: the socket is released only when no goroutine holds it, and until then a new dial still
// connects (seen on Windows). Callers reopen the port and rely on refused dials (#146).
func (l *inboundListener) Close() error {
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return l.ln.Close()
	}
	l.stopping = true
	l.mu.Unlock()
	close(l.closing)
	err := l.ln.Close()
	select {
	case <-l.drained:
	case <-time.After(time.Second): // never hold a caller (some hold kd.mu) on a stuck accept
	}
	// Only now does Accept report the close, as on *net.TCPListener: a caller that saw net.ErrClosed may
	// reopen the port at once.
	l.mu.Lock()
	parked := l.parked
	l.parked = nil
	l.closed = true
	l.notifyLocked()
	l.mu.Unlock()
	for _, p := range parked {
		_ = p.c.Close()
	}
	return err
}

func (l *inboundListener) notifyLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *inboundListener) opError(err error) error {
	return &net.OpError{Op: "accept", Net: l.ln.Addr().Network(), Addr: l.ln.Addr(), Err: err}
}

// drain accepts until the listener closes. Each connection gets a sniff slot first, so a flood holds at
// most maxSniffing sockets here and the rest wait in the kernel queue.
func (l *inboundListener) drain() {
	defer close(l.drained)
	var backoff time.Duration
	for {
		select {
		case l.sniffing <- struct{}{}:
		case <-l.closing:
			return
		}
		c, err := l.ln.Accept()
		if err != nil {
			<-l.sniffing
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Out of file descriptors and the like: wait, as net/http does, then drain again.
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		go func() {
			defer func() { <-l.sniffing }()
			l.sniff(c)
		}()
	}
}

// sniff reads a connection's first five bytes and parks it when they can start a peer exchange.
func (l *inboundListener) sniff(c net.Conn) {
	var head [5]byte
	_ = c.SetReadDeadline(time.Now().Add(l.sniffWait))
	if _, err := io.ReadFull(c, head[:]); err != nil || !peerShaped(head) {
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	l.park(&sniffedConn{Conn: c, head: append([]byte(nil), head[:]...)})
}

func (l *inboundListener) park(c net.Conn) {
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		_ = c.Close()
		return
	}
	var evicted net.Conn
	if len(l.parked) >= maxParked {
		evicted = l.parked[0].c
		l.parked = l.parked[1:]
	}
	l.parked = append(l.parked, parkedConn{c: c, at: time.Now()})
	l.notifyLocked()
	l.mu.Unlock()
	if evicted != nil {
		_ = evicted.Close()
	}
}

// peerShaped reports whether five bytes can start one of the two exchanges a peer opens with: the PQC
// handshake (a 4-byte big-endian length of at most 64 KiB, then its JSON object) or the local-mode key
// exchange (bare JSON, session.ExchangePublicKeysLocal). HTTP, TLS, SSH, RDP and SMB probes fail it.
func peerShaped(b [5]byte) bool {
	switch b[0] {
	case 0:
		n := binary.BigEndian.Uint32(b[:4])
		return n > 0 && n <= maxHandshakeLen && b[4] == '{'
	case '{':
		return b[1] == '"'
	}
	return false
}

// sniffedConn returns the sniffed bytes before the connection's own.
type sniffedConn struct {
	net.Conn
	mu   sync.Mutex
	head []byte
}

func (c *sniffedConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if len(c.head) > 0 {
		n := copy(b, c.head)
		c.head = c.head[n:]
		c.mu.Unlock()
		return n, nil
	}
	c.mu.Unlock()
	return c.Conn.Read(b)
}
