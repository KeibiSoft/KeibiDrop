// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Guards the joiner's bridge leg against a creator that reaches the bridge
// ABOUTME: late, after its direct dial to the joiner's blocked inbound timed out.

package common

import (
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// Creator open, joiner blocked, the always-on box against a laptop at home. The
// creator accepts the joiner's direct dial, learns nothing about the joiner's
// inbound from it, and dials that inbound for the full DirectDialTimeout before
// it takes the bridge. The joiner's leg must outlive that, plus one direct accept
// window the creator may still be inside, or the two never meet on the bridge.
func TestJoinBridgeWait_CoversTheCreatorsDoomedDirectDial(t *testing.T) {
	require.GreaterOrEqual(t, joinBridgeWait, session.DirectDialTimeout+bridgeRoundWait,
		"the joiner gives up before a creator with an open inbound can reach the bridge")
}

// The 0.4.2 hardening put the accept-loop first-byte bound (3 s) on the joiner's
// bridge leg. Measured on the Timisoara container against a Mac at home: the
// creator arrived at 15.2 s, the joiner had left at 3 s, and the creator then
// held a phantom session on the joiner's buffered hello. A hello that lands
// after that bound but inside the joiner's window must connect.
func TestJoinBridgeInbound_WaitsForALateCreator(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a few seconds")
	}
	kd := newRendezvousKD(t)
	creator, err := session.InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)
	creator.PeerPubKeys = &kbc.PeerKeys{
		X25519Public: kd.session.OwnKeys.X25519Public,
		MlKemPublic:  kd.session.OwnKeys.MlKemPublic,
	}
	creator.ExpectedPeerFingerprint = kd.session.OwnFingerprint
	kd.session.ExpectedPeerFingerprint = creator.OwnFingerprint

	joinerLeg, creatorLeg := net.Pipe()
	t.Cleanup(func() { joinerLeg.Close(); creatorLeg.Close() })

	const lateBy = 4 * time.Second // past the 3 s accept-loop bound, inside the window
	creatorDone := make(chan error, 1)
	go func() {
		time.Sleep(lateBy)
		creatorDone <- session.PerformOutboundHandshakeOnConn(creator, creatorLeg)
	}()

	require.NoError(t, kd.joinBridgeInbound(joinerLeg),
		"the joiner must hold its bridge leg while the creator finishes its doomed direct dial")
	require.NoError(t, <-creatorDone)
}
