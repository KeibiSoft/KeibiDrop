// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Inbound reachability hint: learn that nothing reaches our listener and publish it
// ABOUTME: in the encrypted registration, so neither peer waits out a doomed dial or accept.

package common

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// This host cannot tell whether the internet reaches its listener. The relay answers
// with a dial-back via its /probe endpoint. The verdict rides the encrypted
// registration, so the peer skips the doomed dial. A relay without /probe costs nothing.

const probeTimeout = 6 * time.Second

// probeCacheTTL expires a verdict even when the local address is unchanged. A router
// swap changes reachability while the ISP keeps the same prefix. A stale "blocked"
// cannot self-correct, because no inbound can disprove it. Only a re-probe clears it.
const probeCacheTTL = 15 * time.Minute

// relayProbeResult is the relay's /probe answer.
type relayProbeResult struct {
	Reachable bool `json:"reachable"`
}

// ProbeInboundReachability asks the relay to dial the local listener and caches the
// verdict per local address for probeCacheTTL. Any failure leaves the mark unchanged.
func (kd *KeibiDrop) ProbeInboundReachability(ctx context.Context) {
	// Snapshot the address up front. The verdict must carry the address it was measured
	// on, not the address after the probe ends. A mid-probe network change would
	// otherwise pin the new network to the relay for probeCacheTTL.
	addr := kd.LocalIPv6IP
	on, ok := kd.probedOn.Load().(string)
	if ok && on == addr && time.Since(time.Unix(0, kd.probedAt.Load())) < probeCacheTTL {
		return
	}
	logger := kd.logger.With("method", "probe-inbound")

	u, err := kd.relayURL("probe")
	if err != nil {
		return
	}
	q := u.Query()
	q.Set("port", strconv.Itoa(kd.inboundPort))
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return
	}
	resp, err := kd.relayClient.Do(req)
	if err != nil {
		logger.Debug("Inbound probe unavailable; reachability stays as observed", "error", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// The relay answered, so cache either way. A relay without the endpoint answers 404
	// every time, and a re-ask would add a round trip to every registration.
	kd.probedOn.Store(addr)
	kd.probedAt.Store(time.Now().UnixNano())
	kd.probedReachable.Store(false)

	if resp.StatusCode != http.StatusOK {
		logger.Debug("Relay does not offer an inbound probe", "status", resp.StatusCode)
		return
	}
	var result relayProbeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}
	if result.Reachable {
		kd.probedReachable.Store(true)
		kd.markInboundReachable()
		return
	}
	kd.markInboundBlockedOn(addr)
	logger.Info("Relay could not reach our listener; advertising a blocked inbound so the peer skips its direct dial",
		"port", kd.inboundPort)
}

// markInboundBlocked records a timed-out accept on the current local address.
func (kd *KeibiDrop) markInboundBlocked() {
	kd.markInboundBlockedOn(kd.LocalIPv6IP)
}

// markInboundBlockedOn records a timed-out accept together with the address that saw it.
func (kd *KeibiDrop) markInboundBlockedOn(addr string) {
	kd.inboundBlockedOn.Store(addr)
	kd.inboundBlocked.Store(true)
}

// markInboundReachable clears the mark after an inbound connection arrives.
func (kd *KeibiDrop) markInboundReachable() {
	kd.inboundBlocked.Store(false)
}

// InboundBlocked reports whether to advertise the listener as unreachable. The mark
// binds to the address that observed it. A new address is a new network and re-probes,
// so one bad network cannot pin the node to the relay.
func (kd *KeibiDrop) InboundBlocked() bool {
	if !kd.inboundBlocked.Load() {
		return false
	}
	if on, _ := kd.inboundBlockedOn.Load().(string); on != kd.LocalIPv6IP {
		kd.inboundBlocked.Store(false)
		return false
	}
	return true
}

// PeerInboundBlocked reports whether the peer advertised its listener as unreachable.
func (kd *KeibiDrop) PeerInboundBlocked() bool { return kd.peerInboundBlocked.Load() }

// probeSaysReachable reports a fresh relay verdict that the listener is reachable on
// the current address.
func (kd *KeibiDrop) probeSaysReachable() bool {
	on, ok := kd.probedOn.Load().(string)
	return ok && on == kd.LocalIPv6IP && kd.probedReachable.Load() &&
		time.Since(time.Unix(0, kd.probedAt.Load())) < probeCacheTTL
}

// noteEmptyAcceptWindow records that a direct accept window closed with nobody in it.
// Without a probe verdict that is the only evidence there is, so it marks the
// listener blocked. Against a fresh reachable verdict it is not evidence: the peer
// was not dialing yet. An always-on peer waits through many empty windows, and a
// mark here would pin it to the bridge, because the hint stops the peer from dialing
// and only an arriving dial clears the mark.
func (kd *KeibiDrop) noteEmptyAcceptWindow() {
	if kd.probeSaysReachable() {
		return
	}
	kd.markInboundBlocked()
}
