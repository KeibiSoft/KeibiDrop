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

// Nothing on this host can tell whether the internet reaches our own listener, so the relay
// answers it by dialing us back (its /probe endpoint). The verdict rides the encrypted
// registration so the peer skips the doomed dial too. A relay without /probe costs nothing.

const probeTimeout = 6 * time.Second

// probeCacheTTL expires a verdict even when the local address is unchanged: swapping a router
// or its firewall changes reachability while the ISP re-delegates the same prefix. A stale
// "blocked" cannot self-correct (we tell the peer not to dial and skip our accept, so no
// inbound can disprove it), so only re-probing clears it.
const probeCacheTTL = 15 * time.Minute

// relayProbeResult is the relay's /probe answer.
type relayProbeResult struct {
	Reachable bool `json:"reachable"`
}

// ProbeInboundReachability asks the relay to dial our listener and records the verdict, cached
// per local address for probeCacheTTL. Best effort: any failure leaves the mark as it was.
func (kd *KeibiDrop) ProbeInboundReachability(ctx context.Context) {
	on, ok := kd.probedOn.Load().(string)
	if ok && on == kd.LocalIPv6IP && time.Since(time.Unix(0, kd.probedAt.Load())) < probeCacheTTL {
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

	// The relay answered, so cache either way: a relay without the endpoint answers 404 every
	// time, and re-asking on every registration would just add a round trip to each one.
	kd.probedOn.Store(kd.LocalIPv6IP)
	kd.probedAt.Store(time.Now().UnixNano())

	if resp.StatusCode != http.StatusOK {
		logger.Debug("Relay does not offer an inbound probe", "status", resp.StatusCode)
		return
	}
	var result relayProbeResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return
	}
	if result.Reachable {
		kd.markInboundReachable()
		return
	}
	kd.markInboundBlocked()
	logger.Info("Relay could not reach our listener; advertising a blocked inbound so the peer skips its direct dial",
		"port", kd.inboundPort)
}

// markInboundBlocked records a timed-out accept, tagged with the address it was seen on.
func (kd *KeibiDrop) markInboundBlocked() {
	kd.inboundBlockedOn.Store(kd.LocalIPv6IP)
	kd.inboundBlocked.Store(true)
}

// markInboundReachable clears the mark after an inbound connection actually arrived.
func (kd *KeibiDrop) markInboundReachable() {
	kd.inboundBlocked.Store(false)
}

// InboundBlocked reports whether to advertise our listener as unreachable. Bound to the
// address it was observed on: a different address is a different network, so it re-probes
// rather than letting one bad network pin us to the relay.
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
