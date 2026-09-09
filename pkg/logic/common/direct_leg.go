// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: The mixed leg shape: a joiner behind a blocked inbound keeps its direct
// ABOUTME: outbound (it carries its reads) and only the return leg rides the bridge.

package common

import "errors"

// Connection modes of the mixed shape. direct-out is the joiner's view: its outbound
// leg (its reads) is direct and its inbound comes from the bridge. direct-in is the
// creator's: it accepted the direct leg and dialed the bridge for its outbound.
const (
	ModeDirectOut = "direct-out"
	ModeDirectIn  = "direct-in"
)

var errPeerInboundBlocked = errors.New("joiner advertises a blocked inbound")

// mixedLegs reports the mixed shape on the live mode.
func (kd *KeibiDrop) mixedLegs() bool {
	return kd.ConnectionMode == ModeDirectOut || kd.ConnectionMode == ModeDirectIn
}

// PeerMixedLegs reports that the creator's registration says it keeps a direct leg
// when the joiner's inbound is blocked. A creator without it gets the old behaviour:
// both legs on the bridge.
func (kd *KeibiDrop) PeerMixedLegs() bool { return kd.peerMixedLegs.Load() }

// setModeAfterReconnect keeps ConnectionMode truthful after a reconnect: a session that
// came back through the bridge is a bridge session now (the QUIC lane, the relay
// accounting and the slow-lane notice follow the mode), a mixed one that came back
// mixed keeps its shape. The order of the next outage's attempts follows the shape
// the session was made with (connectMode), not this, so a fallback is retried.
func (kd *KeibiDrop) setModeAfterReconnect(transport string) {
	var mode string
	switch transport {
	case "bridge":
		mode = "bridge"
	case "direct":
		if kd.IsLocalMode {
			mode = "lan"
		} else {
			mode = "direct"
		}
	default:
		return // "" before any reconnect; "mixed" keeps direct-out or direct-in.
	}
	if mode == kd.ConnectionMode {
		return
	}
	kd.logger.Info("Connection mode changed by the reconnect", "from", kd.ConnectionMode, "to", mode)
	kd.ConnectionMode = mode
	kd.emitEvent("connection_mode:" + mode)
}
