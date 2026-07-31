// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Transport is the interface both wire protocols implement, plus the
// ABOUTME: QUIC half (quicTransport, QUIC()) that production dials and listens on.

package transport

import (
	"context"
	"net"
	"time"

	"github.com/quic-go/quic-go"
)

// Transport establishes gRPC-ready net.Conns over a wire protocol. Two implementations:
// TCP (the fast bulk data plane) and QUIC (migration-capable: the always-on control
// channel and the degraded-link fallback). One interface covers both uniformly.
type Transport interface {
	// Dial connects to addr and returns a net.Conn ready to carry gRPC.
	Dial(ctx context.Context, addr string) (net.Conn, error)
	// Listen returns a net.Listener yielding gRPC-ready conns.
	Listen(addr string) (net.Listener, error)
}

// QUIC returns a QUIC transport (one bidirectional stream per connection = one
// net.Conn, mirroring KeibiDrop's per-direction socket).
func QUIC() Transport { return quicTransport{} }

type quicTransport struct{}

func (quicTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	conn, err := quic.DialAddr(ctx, addr, clientTLSConfig(), quicConfig())
	if err != nil {
		return nil, err
	}
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, err
	}
	return newQUICConn(conn, stream), nil
}

func (quicTransport) Listen(addr string) (net.Listener, error) {
	// Own the UDP socket and Transport explicitly: quic.ListenAddr's listener Close does
	// not release its internal socket. Explicit ownership frees the port on close, which
	// reconnect on the same port requires.
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(serverTLSConfig(), quicConfig())
	if err != nil {
		_ = tr.Close()
		_ = udp.Close()
		return nil, err
	}
	return newQUICListener(ln, tr, udp), nil
}

// quicConfig applies to both ends so throughput is not window-limited. MaxIdleTimeout
// is the dead-peer-detection window; KeepAlivePeriod keeps healthy connections alive
// well within it.
func quicConfig() *quic.Config {
	return &quic.Config{
		MaxIdleTimeout:                 20 * time.Second,
		KeepAlivePeriod:                5 * time.Second,
		InitialStreamReceiveWindow:     1 << 20,
		MaxStreamReceiveWindow:         16 << 20,
		InitialConnectionReceiveWindow: 1 << 20,
		MaxConnectionReceiveWindow:     32 << 20,
	}
}

// newUDPTransport binds a local UDP socket toward serverAddr and wraps it in an explicit
// quic.Transport (required for migration). Loopback targets bind to the loopback of the
// same family (an IPv4 socket cannot reach ::1); others bind to the unspecified address.
func newUDPTransport(serverAddr *net.UDPAddr) (*quic.Transport, error) {
	var bind *net.UDPAddr
	if serverAddr.IP != nil && serverAddr.IP.IsLoopback() {
		if serverAddr.IP.To4() != nil {
			bind = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
		} else {
			bind = &net.UDPAddr{IP: net.IPv6loopback}
		}
	}
	udp, err := net.ListenUDP("udp", bind)
	if err != nil {
		return nil, err
	}
	return &quic.Transport{Conn: udp}, nil
}
