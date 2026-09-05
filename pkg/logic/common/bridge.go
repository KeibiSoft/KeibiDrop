// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
package common

import (
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// bridgeRoomToken computes a deterministic 32-byte room token from two
// fingerprints and a direction label. Both peers compute the same token
// for a direction. The creator and joiner roles do not change the token.
func bridgeRoomToken(ownFingerprint, peerFingerprint, direction string) [32]byte {
	fps := []string{ownFingerprint, peerFingerprint}
	sort.Strings(fps)
	return sha256.Sum256([]byte(fps[0] + fps[1] + direction))
}

// bridgeHintApexes are the only domains a relay-suggested bridge may live under.
var bridgeHintApexes = []string{"keibidrop.com", "keibisoft.com"}

// prepareBridgePolicy runs at room entry, before any relay exchange. Strict mode
// clears the bridge address here, so every fallback site downstream sees no
// bridge configured. The base address is the pre-hint value that a failed
// bridge dial falls back to.
func (kd *KeibiDrop) prepareBridgePolicy(logger *slog.Logger) {
	if kd.StrictMode && kd.BridgeAddr != "" {
		logger.Info("Strict mode: data relay fallback disabled")
		kd.BridgeAddr = ""
	}
	kd.bridgeBase = kd.BridgeAddr
	kd.bridgeFellBack.Store(false)
	kd.resetTokenSession()
}

// acceptBridgeHint applies a relay-suggested bridge address. The field rides
// outside the encrypted blob, so a hostile relay could steer both peers to an
// arbitrary host: accept only allowlisted hosts on peer-range ports. Never
// apply a hint while a session is up, because a live session that changes its
// dial target reconnects against a different bridge than its peer.
func (kd *KeibiDrop) acceptBridgeHint(hint string, logger *slog.Logger) {
	if hint == "" || hint == kd.BridgeAddr || kd.StrictMode {
		return
	}
	kd.mu.Lock()
	established := kd.ReconnectManager != nil
	kd.mu.Unlock()
	if established {
		logger.Debug("Bridge hint ignored mid-session", "hint", hint)
		return
	}
	if !validBridgeHintAddr(hint) {
		logger.Warn("Bridge hint rejected", "hint", hint)
		return
	}
	logger.Info("Relay assigned bridge", "bridge", hint)
	kd.BridgeAddr = hint
}

// validBridgeHintAddr accepts host:port with the host under an allowlisted
// apex and the port inside the peer range.
func validBridgeHintAddr(addr string) bool {
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || !config.ValidPeerPort(port) {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, apex := range bridgeHintApexes {
		if host == apex || strings.HasSuffix(host, "."+apex) {
			return true
		}
	}
	return false
}

// effectiveBridgeAddr is the address every bridge lane must dial. After a
// failover it is the base address, so TCP pairs, the UDP lane, and reconnects
// all pair on the same bridge.
func (kd *KeibiDrop) effectiveBridgeAddr() string {
	if kd.bridgeFellBack.Load() && kd.bridgeBase != "" {
		return kd.bridgeBase
	}
	return kd.BridgeAddr
}

// dialBridgeDir connects to the bridge relay and sends a direction-tagged room
// token. One failed dial gets one more try: the same address for a transient
// blip, or the base address when an assigned bridge is unreachable. A failover
// sticks for the session. Both peers converge only when both fail the assigned
// dial (bridge down); one-sided reachability still fails the connect.
func (kd *KeibiDrop) dialBridgeDir(direction string, logger *slog.Logger) (net.Conn, error) {
	addr := kd.effectiveBridgeAddr()
	conn, err := kd.dialBridgeAddr(addr, direction, logger)
	if err == nil {
		return conn, nil
	}

	if kd.bridgeBase == "" || kd.bridgeBase == addr {
		return kd.dialBridgeAddr(addr, direction, logger)
	}

	logger.Warn("Assigned bridge unreachable, falling back to base bridge",
		"assigned", addr, "base", kd.bridgeBase, "error", err)
	conn, fbErr := kd.dialBridgeAddr(kd.bridgeBase, direction, logger)
	if fbErr != nil {
		return nil, fmt.Errorf("assigned bridge: %v; base bridge: %w", err, fbErr)
	}
	kd.bridgeFellBack.Store(true)
	return conn, nil
}

// dialBridgeAddr dials one bridge address and sends the room token. Separate
// tokens for the two directions prevent the bridge from pairing two
// connections from the same peer. A funded session on a KeibiSoft bridge
// sends the paid preamble instead - the same single write - and the returned
// conn strips the bridge's one ack byte on first read, so payment adds no
// round trip anywhere.
func (kd *KeibiDrop) dialBridgeAddr(addr, direction string, logger *slog.Logger) (net.Conn, error) {
	conn, err := session.DialWithStableAddr("tcp", addr, 15*time.Second, logger)
	if err != nil {
		return nil, fmt.Errorf("dial bridge: %w", err)
	}

	ownFP := kd.session.OwnFingerprint
	peerFP := kd.session.ExpectedPeerFingerprint
	token := bridgeRoomToken(ownFP, peerFP, direction)

	ts := kd.tokenSessionFor(addr, logger)
	payload := token[:]
	if ts != nil {
		payload = buildPayPreamble(ts.anchorBytes(), token)
	}

	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("send room token: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})

	logger.Info("Bridge room token sent", "dir", direction, "paid", ts != nil,
		"token", fmt.Sprintf("%x..%x", token[:4], token[28:]))
	if ts != nil {
		return newPayConn(conn, ts), nil
	}
	return conn, nil
}

// bridgeInbound runs the creator's inbound leg on the bridge and reports whether a
// joiner completed the handshake on it. It reads the leg parked by the previous
// round first, then dials a fresh one.
//
// A leg that saw no joiner is parked open, not closed. The bridge keeps an
// unmatched room until its own expiry whether or not the client is still there,
// so a closed leg leaves a corpse that the next joiner is paired with, and that
// joiner reads EOF. Measured on the Timisoara bridge: the joiner arrived 5 s after
// the creator closed its leg, matched the corpse, and both sides waited out their
// windows alone. An open leg stays a live half: the joiner's hello lands in it
// while the creator runs its direct window, and the next bridge phase reads it.
func (kd *KeibiDrop) bridgeInbound(logger *slog.Logger) bool {
	if parked := kd.takeParkedBridgeIn(); parked != nil {
		if err := session.PerformInboundHandshakeWait(kd.session, parked, bridgeRoundWait); err == nil {
			logger.Info("Joiner arrived on the bridge leg parked from the last round")
			return true
		} else {
			parked.Close()
			kd.session.ResetInboundCrypto()
			logger.Info("Parked bridge leg had no joiner", "error", err)
		}
	}
	inConn, err := kd.dialBridgeDir("pair1", logger)
	if err != nil {
		logger.Warn("Bridge dial (inbound) failed this round", "error", err)
		return false
	}
	// Bound the wait for the joiner to the round. Without a deadline this blocks
	// until the REMOTE bridge closes the unpaired room, which is what produced the
	// +38s EOF and made the round length a property of the bridge, not of us. The
	// handshake clears the deadline itself once the conn becomes the transport.
	if err := session.PerformInboundHandshakeWait(kd.session, inConn, bridgeRoundWait); err != nil {
		kd.session.ResetInboundCrypto()
		kd.parkBridgeIn(inConn)
		logger.Info("No joiner on the bridge this round", "error", err)
		return false
	}
	return true
}

// parkBridgeIn keeps a bridge inbound leg open for the next round. Any leg already
// parked is closed: the bridge has expired its room by now.
func (kd *KeibiDrop) parkBridgeIn(c net.Conn) {
	kd.mu.Lock()
	old := kd.parkedBridgeIn
	kd.parkedBridgeIn = c
	kd.mu.Unlock()
	if old != nil {
		old.Close()
	}
}

// takeParkedBridgeIn hands the parked leg to the caller and forgets it.
func (kd *KeibiDrop) takeParkedBridgeIn() net.Conn {
	kd.mu.Lock()
	defer kd.mu.Unlock()
	c := kd.parkedBridgeIn
	kd.parkedBridgeIn = nil
	return c
}

// closeParkedBridgeIn drops a leg nobody will read again: the create ended.
func (kd *KeibiDrop) closeParkedBridgeIn() {
	if c := kd.takeParkedBridgeIn(); c != nil {
		c.Close()
	}
}
