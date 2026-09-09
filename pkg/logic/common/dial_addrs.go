// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Direct dial over every advertised address: IPv6 first, then the public IPv4
// ABOUTME: the relay saw the peer on, so a NAS with a forwarded port gets a direct leg.

package common

import (
	"errors"
	"log/slog"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// errNoDirectAddress says the peer advertised nothing worth a direct dial.
var errNoDirectAddress = errors.New("peer advertises no reachable direct address")

// preferIPv4 puts the peer's IPv4 first, for a network whose IPv6 is present
// but broken, where every IPv6 dial waits out DirectDialTimeout first.
var preferIPv4 = os.Getenv("KEIBIDROP_PREFER_IPV4") == "1"

// listen4Hint is the IPv4 half of the registration: the address the relay saw
// this host on and its verdict on the listener there. Nil before a probe
// answered over IPv4. Behind a NAT this host cannot see that address itself.
func (kd *KeibiDrop) listen4Hint() *ConnectionHint {
	v4 := kd.PublicIPv4()
	if v4 == "" {
		return nil
	}
	return &ConnectionHint{
		IP:             v4,
		Proto:          "tcp",
		Port:           kd.inboundPort,
		InboundBlocked: !kd.inbound4Reachable.Load(),
	}
}

// peerDialAddrs lists the peer's advertised addresses worth a direct dial, in
// order: IPv6, the historical path, then the public IPv4 from its registration.
// A family whose inbound the relay could not reach is left out; the dial would
// only burn its timeout.
func (kd *KeibiDrop) peerDialAddrs(port int) []string {
	var v6, v4 []string
	if kd.PeerIPv6IP != "" && !kd.PeerInboundBlocked() {
		v6 = append(v6, net.JoinHostPort(kd.PeerIPv6IP, strconv.Itoa(port)))
	}
	if kd.PeerIPv4IP != "" && !kd.peerInbound4Blocked.Load() {
		v4 = append(v4, net.JoinHostPort(kd.PeerIPv4IP, strconv.Itoa(port)))
	}
	if preferIPv4 {
		return append(v4, v6...)
	}
	return append(v6, v4...)
}

// A direct dial that fails fast (refused, reset, closed before the leg
// acknowledgement) is a creator between two rounds or a backlog the creator
// has not drained yet, not a blocked path: it is dialed again after a short
// wait. A dial that fails slowly (the SYN blackholed until DirectDialTimeout)
// is a blocked path and moves on. Before this, one such hiccup sent the whole
// session to the bridge (seen 2026-09-09 20:49, BUGS 33). Variables, so the
// tests can shrink them.
var (
	directDialAttempts  = 3
	directDialRetryWait = 500 * time.Millisecond
	directDialFastFail  = 2 * time.Second
)

// dialPeerDirect runs the outbound handshake against each address in turn and
// keeps the first that completes it. A fast failure is retried on the same
// address; between attempts the half-built outbound state is dropped, as the
// bail-out paths do; after the last failure it is left as it was for the
// fallback that follows.
func (kd *KeibiDrop) dialPeerDirect(logger *slog.Logger, addrs []string) error {
	if len(addrs) == 0 {
		return errNoDirectAddress
	}
	var err error
	first := true
	for _, addr := range addrs {
		for attempt := 1; attempt <= directDialAttempts; attempt++ {
			if !first {
				kd.dropOutboundConn()
				kd.session.ResetOutboundCrypto()
			}
			first = false
			began := time.Now()
			if err = session.PerformOutboundHandshake(kd.session, addr); err == nil {
				host, _, _ := net.SplitHostPort(addr)
				kd.peerDialedIP.Store(host)
				return nil
			}
			if time.Since(began) >= directDialFastFail || attempt == directDialAttempts {
				break // A blocked path, or out of attempts: the next address, if any.
			}
			wait := time.Duration(attempt) * directDialRetryWait
			logger.Info("Direct dial failed fast, dialing again", "addr", addr, "attempt", attempt, "in", wait, "error", err)
			time.Sleep(wait)
		}
		logger.Info("Direct dial failed", "addr", addr, "error", err)
	}
	return err
}

// peerDirectIP is the peer address a reconnect dials: the family that answered
// the direct dial, at the address the peer advertises now, else what is known.
func (kd *KeibiDrop) peerDirectIP() string {
	dialed, _ := kd.peerDialedIP.Load().(string)
	switch {
	case dialed != "" && isIPv4(dialed) && kd.PeerIPv4IP != "":
		return kd.PeerIPv4IP
	case dialed != "" && !isIPv4(dialed) && kd.PeerIPv6IP != "":
		return kd.PeerIPv6IP
	case dialed != "":
		return dialed
	case kd.PeerIPv6IP != "":
		return kd.PeerIPv6IP
	}
	return kd.PeerIPv4IP
}

func isIPv4(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}
