// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: UDP relay client for the QUIC control lane: registers a room token with the
// ABOUTME: bridge's UDP port, then hands the paired socket to QUIC.

package common

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// udpRelayMagic prefixes a registration datagram. The relay echoes it as the pairing ACK.
// The first byte clears QUIC's fixed bit (0x40), which every QUIC packet sets, so
// registrations and relayed traffic never parse as each other.
var udpRelayMagic = []byte("\x00KDUDPRELAY1")

// Resend until the relay ACKs. Give up after the budget, so a relay without a UDP
// room fails fast to TCP-only.
const (
	udpRelayResend = 500 * time.Millisecond
	udpRelayBudget = 10 * time.Second
)

// errNoPairingAck means the room did not pair inside the budget, which usually means the
// counterpart has not registered yet. It is the ordinary "keep waiting" outcome, not a
// fault, so callers retry it at once instead of backing off.
var errNoPairingAck = errors.New("no pairing ack")

// roomSocket owns the UDP socket for one relay room across the attempts of one channel
// generation.
//
// The bridge pairs any two DISTINCT source addresses that present the same room token; it
// cannot tell which peer an address belongs to. A client that binds a fresh socket per
// attempt and abandons the old one therefore pairs with ITS OWN dead socket as soon as the
// bridge still holds that corpse as a waiter, hands it to QUIC, and times out against
// silence. Measured on 2026-08-16: 39 such self-pairs against 6 real ones.
//
// One socket per room removes the possibility. A same-address re-registration only
// refreshes the bridge's waiter, so there is never a second address of ours to pair with.
type roomSocket struct {
	kd        *KeibiDrop
	direction string
	udp       *net.UDPConn
	relayAddr *net.UDPAddr
	paired    bool // The relay acked this socket. Only a paired socket may be poisoned.
}

func (kd *KeibiDrop) newRoomSocket(direction string) *roomSocket {
	return &roomSocket{kd: kd, direction: direction}
}

// pair registers with the relay and returns the socket once the room pairs. It KEEPS the
// socket when registration times out, so the next attempt re-registers from the same
// address. The caller must call poison after a paired-then-failed handshake, and release
// once the socket belongs to QUIC.
func (r *roomSocket) pair(ctx context.Context, s *session.Session, logger *slog.Logger) (*net.UDPConn, *net.UDPAddr, error) {
	if r.udp == nil {
		bridgeAddr := r.kd.effectiveBridgeAddr()
		relayAddr, err := net.ResolveUDPAddr("udp", bridgeAddr)
		if err != nil {
			return nil, nil, fmt.Errorf("resolve relay %s: %w", bridgeAddr, err)
		}
		udp, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, nil, fmt.Errorf("udp relay socket: %w", err)
		}
		r.udp, r.relayAddr = udp, relayAddr
	}

	// The local port is what matches this socket against the bridge's A=/B= pair log.
	// Without it a self-pair against our own dead socket is invisible from the client.
	localPort := 0
	if la, ok := r.udp.LocalAddr().(*net.UDPAddr); ok {
		localPort = la.Port
	}
	logger = logger.With("dir", r.direction, "local_port", localPort)

	token := bridgeRoomToken(s.OwnFingerprint, s.ExpectedPeerFingerprint, r.direction)
	reg := append(append([]byte(nil), udpRelayMagic...), token[:]...)
	if ts := r.kd.currentTokenSession(); ts != nil {
		// Funded session: the anchor rides the registration so the UDP lane
		// joins the same paid class and coverage as the TCP pairs.
		anchor := ts.anchorBytes()
		reg = append(reg, anchor[:]...)
	}
	if err := registerUDPRelay(ctx, r.udp, r.relayAddr, reg); err != nil {
		return nil, nil, err // Keep the socket: the next attempt refreshes the same waiter.
	}
	r.paired = true

	logger.Info("UDP relay room paired", "relay", r.relayAddr,
		"token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	return r.udp, r.relayAddr, nil
}

// poison drops a socket whose room PAIRED but whose handshake failed. The bridge keeps a
// paired source paired and only re-acks its registrations, so staying on this socket would
// wedge the client against the corpse until the room's idle TTL. The next pair rebinds.
//
// An UNPAIRED socket is kept instead. It is only a waiter at the bridge, and rebinding
// would hand the bridge a second address of ours for one token, which is the self-pair
// this type exists to prevent.
func (r *roomSocket) poison() {
	if !r.paired {
		return
	}
	r.drop()
}

// release gives up ownership after the socket is handed to QUIC, which closes it.
func (r *roomSocket) release() {
	r.udp = nil
	r.paired = false
}

// close releases a socket the caller still owns, paired or not.
func (r *roomSocket) close() { r.drop() }

func (r *roomSocket) drop() {
	if r.udp != nil {
		_ = r.udp.Close()
		r.udp = nil
	}
	r.paired = false
}

// registerUDPRelay resends until the relay echoes the magic back. The echo means the
// counterpart registered the same token. The loop drops earlier datagrams: only the
// peer's QUIC arrives that early, and QUIC retransmits.
func registerUDPRelay(ctx context.Context, udp *net.UDPConn, relayAddr *net.UDPAddr, reg []byte) error {
	deadline := time.Now().Add(udpRelayBudget)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	buf := make([]byte, 1500)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := udp.WriteToUDP(reg, relayAddr); err != nil {
			return fmt.Errorf("udp relay register: %w", err)
		}
		_ = udp.SetReadDeadline(time.Now().Add(udpRelayResend))
		n, src, err := udp.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue // Resend window elapsed. The relay may still wait for the peer.
			}
			return fmt.Errorf("udp relay read: %w", err)
		}
		if sameUDPAddr(src, relayAddr) && bytes.Equal(buf[:n], udpRelayMagic) {
			_ = udp.SetReadDeadline(time.Time{}) // QUIC gets a deadline-free socket.
			return nil
		}
	}
	return fmt.Errorf("udp relay: %w from %s within %s", errNoPairingAck, relayAddr, udpRelayBudget)
}

// sameUDPAddr checks that the ACK came from the relay. Only the relay can pair the room.
func sameUDPAddr(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
