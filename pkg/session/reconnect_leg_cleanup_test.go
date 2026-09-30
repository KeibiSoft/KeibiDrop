// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// A bridge reconnect attempt that fails on its second leg leaves nothing
// parked at the bridge and nothing half-built on the session, so the next
// attempt cannot be paired with this one's corpse. Seen 2026-09-30 in the
// bridge journal: pair 563 matched two legs of the same daemon, one per
// attempt, and each attempt then waited out a handshake bound talking to
// itself while the phone on the other side never got a leg.

package session

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

func TestReconnectBridge_ResponderDropsItsOutboundLegWhenTheInboundFails(t *testing.T) {
	shrinkHandshakeBounds(t, 60*time.Millisecond, 300*time.Millisecond)
	logger := testkit.DiscardLogger()
	alice, err := InitSession(logger, config.OutboundPort, config.InboundPort)
	require.NoError(t, err)
	bob, err := InitSession(logger, config.OutboundPort+2, config.InboundPort+2)
	require.NoError(t, err)
	alice.PeerPubKeys = &kbc.PeerKeys{X25519Public: bob.OwnKeys.X25519Public, MlKemPublic: bob.OwnKeys.MlKemPublic}
	alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
	require.NoError(t, err)

	// pair1: the bridge takes Alice's hello and relays it to nobody. pair2: the
	// peer never speaks, so the inbound bound expires.
	p1near, p1far := net.Pipe()
	p2near, p2far := net.Pipe()
	defer p2far.Close()
	firstLegEOF := make(chan error, 1)
	go func() {
		_, e := io.Copy(io.Discard, p1far) // returns nil once the near end closes
		firstLegEOF <- e
	}()

	r := NewReconnectManager(alice, logger)
	r.BridgeAddr = "b:26600"
	r.DialBridge = func(dir string) (net.Conn, error) {
		if dir == "pair1" {
			return p1near, nil
		}
		return p2near, nil
	}

	err = r.reconnectBridge(logger, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bridge inbound handshake")

	select {
	case e := <-firstLegEOF:
		require.NoError(t, e, "the first leg is closed, so the bridge drops its room")
	case <-time.After(3 * time.Second):
		t.Fatal("the first leg stayed open after the attempt failed")
	}
	require.Nil(t, alice.OutboundConn(), "the half-built socket is gone from the session")
	require.Nil(t, alice.SEKOutbound, "the outbound key is cleared for the next attempt")
}
