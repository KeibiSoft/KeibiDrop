// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: QUIC over a UDP socket the caller already owns, for the relayed control lane.

package transport

import (
	"context"
	"net"

	"github.com/quic-go/quic-go"
)

// ListenOnConn serves QUIC on an already-bound UDP socket. The relay pairs by source address,
// so the registered socket must be the one QUIC listens on. Closing the listener closes it.
func ListenOnConn(udp net.PacketConn) (net.Listener, error) {
	tr := &quic.Transport{Conn: udp}
	ln, err := tr.Listen(serverTLSConfig(), quicConfig())
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	return newQUICListener(ln, tr, udp), nil
}

// DialOnConn opens a QUIC connection to addr over an already-bound UDP socket. Not migratable
// on purpose: migration binds a fresh socket, which the relay sees as unpaired and drops.
func DialOnConn(ctx context.Context, udp net.PacketConn, addr net.Addr) (net.Conn, error) {
	tr := &quic.Transport{Conn: udp}
	qc, err := tr.Dial(ctx, addr, clientTLSConfig(), quicConfig())
	if err != nil {
		_ = tr.Close()
		return nil, err
	}
	st, err := qc.OpenStreamSync(ctx)
	if err != nil {
		_ = qc.CloseWithError(0, "")
		_ = tr.Close()
		return nil, err
	}
	return &ownedConn{Conn: newQUICConn(qc, st), tr: tr, udp: udp}, nil
}

// ownedConn closes the transport and socket it was dialed over; quic-go closes neither.
type ownedConn struct {
	net.Conn
	tr  *quic.Transport
	udp net.PacketConn
}

func (c *ownedConn) Close() error {
	err := c.Conn.Close()
	_ = c.tr.Close()
	_ = c.udp.Close()
	return err
}
