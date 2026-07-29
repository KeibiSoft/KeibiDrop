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

// udpRelayMagic prefixes a registration datagram and is echoed back as the pairing ACK. Its
// first byte has QUIC's fixed bit (0x40) clear, which no valid QUIC packet has, so relayed
// traffic and registrations can never be read as each other.
var udpRelayMagic = []byte("\x00KDUDPRELAY1")

// Best-effort over UDP: resend until the relay ACKs, give up after the budget so a relay
// without a UDP room fails fast to TCP-only.
const (
	udpRelayResend = 500 * time.Millisecond
	udpRelayBudget = 10 * time.Second
)

// dialUDPRelayDir binds a UDP socket and registers direction's room token, returning the socket
// once the room is paired. Every datagram is relayed, so the peer appears at the relay address.
func (kd *KeibiDrop) dialUDPRelayDir(ctx context.Context, s *session.Session, direction string, logger *slog.Logger) (*net.UDPConn, *net.UDPAddr, error) {
	relayAddr, err := net.ResolveUDPAddr("udp", kd.BridgeAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve relay %s: %w", kd.BridgeAddr, err)
	}
	udp, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, nil, fmt.Errorf("udp relay socket: %w", err)
	}

	token := bridgeRoomToken(s.OwnFingerprint, s.ExpectedPeerFingerprint, direction)
	reg := append(append([]byte(nil), udpRelayMagic...), token[:]...)
	if err := registerUDPRelay(ctx, udp, relayAddr, reg); err != nil {
		_ = udp.Close()
		return nil, nil, err
	}

	logger.Info("UDP relay room paired", "dir", direction, "relay", relayAddr,
		"token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	return udp, relayAddr, nil
}

// registerUDPRelay resends until the relay echoes the magic back, its signal that the
// counterpart registered the same token. A datagram arriving before the ACK is dropped: only
// the peer's QUIC can be here that early, and QUIC retransmits.
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
				continue // resend window elapsed; the relay may still be waiting for the peer
			}
			return fmt.Errorf("udp relay read: %w", err)
		}
		if sameUDPAddr(src, relayAddr) && bytes.Equal(buf[:n], udpRelayMagic) {
			_ = udp.SetReadDeadline(time.Time{}) // QUIC gets a deadline-free socket
			return nil
		}
	}
	return fmt.Errorf("udp relay: no pairing ack from %s within %s", relayAddr, udpRelayBudget)
}

// sameUDPAddr checks an ACK came from the relay, so only the relay can pair us.
func sameUDPAddr(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
