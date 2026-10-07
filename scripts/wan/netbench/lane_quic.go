// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: QUIC lane. Reuses pkg/transport verbatim (KeibiSoft quic-go fork:
// ABOUTME: darwin sendmsg_x batching, linux GSO), so rows measure the shipped stack.
// ABOUTME: quic-secure adds the production SecureConn AEAD layer over the stream,
// ABOUTME: exactly like the QUIC control lane, to price the double crypto.

package main

import (
	"context"
	"fmt"
	"net"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/KeibiSoft/KeibiDrop/pkg/transport"
)

// benchKey is a fixed key: the lane prices AEAD framing cost, not key secrecy.
var benchKey = func() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}()

func quicListen(addr string) (net.Listener, error) { return transport.QUIC().Listen(addr) }

func quicAccept(ln net.Listener, spec *TrialSpec, timeout time.Duration) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := ln.Accept()
		ch <- res{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		return wrapSecure(r.c, spec, session.NoncePrefixInbound), nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("quic accept timeout")
	}
}

func quicDial(addr string, spec *TrialSpec) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if spec.Lane == "quic-conn" {
		// CONNECTED client socket: skips the per-packet policy evaluation that
		// throttles unconnected sends on macOS (Finding 1). quic.Transport calls
		// WriteTo unconditionally (net.ErrWriteToConnected on connected sockets),
		// so an adapter maps WriteTo onto Write. The adapter hides *net.UDPConn
		// from quic-go, which drops it to the generic conn path (no sendmsg_x /
		// GSO batching): this lane UNDERSTATES the fix, batching would add more.
		raddr, err := net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return nil, err
		}
		uc, err := net.DialUDP("udp", nil, raddr)
		if err != nil {
			return nil, err
		}
		c, err := transport.DialOnConn(ctx, connectedPC{uc}, raddr)
		if err != nil {
			_ = uc.Close()
			return nil, err
		}
		return c, nil
	}
	c, err := transport.QUIC().Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	return wrapSecure(c, spec, session.NoncePrefixOutbound), nil
}

// connectedPC lets quic.Transport drive a connected UDP socket: WriteTo becomes
// Write so every send rides the cached-verdict fast path. It deliberately does
// NOT embed *net.UDPConn: promotion would expose WriteMsgUDP/SyscallConn, quic-go
// would detect OOB capability and call WriteMsgUDP with an addr, which errors on
// connected sockets. Exposing only the plain PacketConn surface forces quic-go's
// generic path through this WriteTo.
type connectedPC struct{ uc *net.UDPConn }

func (c connectedPC) ReadFrom(b []byte) (int, net.Addr, error)  { return c.uc.ReadFrom(b) }
func (c connectedPC) WriteTo(b []byte, _ net.Addr) (int, error) { return c.uc.Write(b) }
func (c connectedPC) Close() error                              { return c.uc.Close() }
func (c connectedPC) LocalAddr() net.Addr                       { return c.uc.LocalAddr() }
func (c connectedPC) SetDeadline(t time.Time) error             { return c.uc.SetDeadline(t) }
func (c connectedPC) SetReadDeadline(t time.Time) error         { return c.uc.SetReadDeadline(t) }
func (c connectedPC) SetWriteDeadline(t time.Time) error        { return c.uc.SetWriteDeadline(t) }

func wrapSecure(c net.Conn, spec *TrialSpec, prefix uint32) net.Conn {
	if spec.Lane != "quic-secure" {
		return c
	}
	return session.NewSecureConn(c, benchKey, kbc.SupportedCiphers()[0], prefix)
}
