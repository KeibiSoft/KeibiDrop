// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: The creator keeps its bridge inbound leg open between rounds, so a joiner
// ABOUTME: never pairs with a corpse room, and the next round reads the parked leg first.

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

// pairedWith returns a loopback pair: the creator's leg (as parked at the bridge)
// and the far end, which stands in for the joiner once the bridge has spliced them.
func pairedWith(t *testing.T) (leg, far net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	leg, err = net.Dial("tcp", ln.Addr().String())
	require.NoError(t, err)
	far = <-accepted
	t.Cleanup(func() { leg.Close(); far.Close() })
	return leg, far
}

func joinerFor(t *testing.T, kd *KeibiDrop) *session.Session {
	t.Helper()
	joiner, err := session.InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)
	joiner.PeerPubKeys = &kbc.PeerKeys{
		X25519Public: kd.session.OwnKeys.X25519Public,
		MlKemPublic:  kd.session.OwnKeys.MlKemPublic,
	}
	joiner.ExpectedPeerFingerprint = kd.session.OwnFingerprint
	kd.session.ExpectedPeerFingerprint = joiner.OwnFingerprint
	return joiner
}

// The joiner arrived while the creator was in its direct window and the bridge
// spliced it onto the parked leg. The next bridge phase must read that leg and
// finish the handshake without dialing a new room.
func TestBridgeInbound_ReadsTheParkedLegFirst(t *testing.T) {
	kd := newRendezvousKD(t)
	kd.BridgeAddr = "" // a fresh dial is impossible; only the parked leg can succeed
	joiner := joinerFor(t, kd)
	leg, far := pairedWith(t)
	kd.parkBridgeIn(leg)

	joinerDone := make(chan error, 1)
	go func() {
		time.Sleep(500 * time.Millisecond)
		joinerDone <- session.PerformOutboundHandshakeOnConn(joiner, far)
	}()

	require.True(t, kd.bridgeInbound(testkit.DiscardLogger()),
		"the parked leg carried the joiner's hello and must complete the handshake")
	require.NoError(t, <-joinerDone)
	require.Nil(t, kd.takeParkedBridgeIn(), "a leg that became the session is no longer parked")
}

// The bridge expired the parked room, so the leg reads EOF. The creator must drop
// it and fall through to a fresh dial instead of reporting a phantom joiner.
func TestBridgeInbound_DropsAnExpiredParkedLeg(t *testing.T) {
	kd := newRendezvousKD(t)
	kd.BridgeAddr = ""
	joinerFor(t, kd)
	leg, far := pairedWith(t)
	kd.parkBridgeIn(leg)
	require.NoError(t, far.Close())

	require.False(t, kd.bridgeInbound(testkit.DiscardLogger()),
		"an EOF on the parked leg is no joiner, and the fresh dial has no bridge to reach")
	require.Nil(t, kd.takeParkedBridgeIn())
	_, err := leg.Write([]byte{0})
	require.Error(t, err, "the expired leg must be closed, not leaked")
}

// A fresh leg that saw no joiner is parked open, not closed, so the bridge room
// it holds stays a live half instead of a corpse. Takes one bridge round.
func TestBridgeInbound_ParksTheLegWhenNobodyComes(t *testing.T) {
	if testing.Short() {
		t.Skip("takes one bridge round")
	}
	kd := newRendezvousKD(t)
	joinerFor(t, kd)

	// A bridge that parks every token and never pairs it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })
	held := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			held <- c
		}
	}()
	kd.BridgeAddr = ln.Addr().String()
	kd.bridgeBase = kd.BridgeAddr

	require.False(t, kd.bridgeInbound(testkit.DiscardLogger()))
	parked := kd.takeParkedBridgeIn()
	require.NotNil(t, parked, "the empty round must park its leg for the next round")
	_, err = parked.Write([]byte{0})
	require.NoError(t, err, "the parked leg must still be open")
	parked.Close()
	for len(held) > 0 {
		(<-held).Close()
	}
}

func TestParkedBridgeLeg_ReplaceClosesTheOldOne(t *testing.T) {
	kd := newRendezvousKD(t)
	first, _ := pairedWith(t)
	second, _ := pairedWith(t)
	kd.parkBridgeIn(first)
	kd.parkBridgeIn(second)
	_, err := first.Write([]byte{0})
	require.Error(t, err, "replacing a parked leg closes the old one")
	require.Same(t, second, kd.takeParkedBridgeIn())
	kd.parkBridgeIn(second)
	kd.closeParkedBridgeIn()
	_, err = second.Write([]byte{0})
	require.Error(t, err, "closing the parked leg closes the socket")
	require.Nil(t, kd.takeParkedBridgeIn())
}
