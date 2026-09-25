// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
//
// A real KeibiDrop session across a REAL deployed bridge, using the production
// token derivation and handshake. Skipped unless KD_LIVE_BRIDGE names one, so
// it never runs in CI or on a laptop by accident:
//
//	KD_LIVE_BRIDGE=bridge.keibisoft.com:26600 go test ./pkg/logic/common/ \
//	  -run TestLiveBridge -count=1 -v
//
// Written 2026-09-19 to check the bridge deployed that day: the wire probe
// proved bytes cross it, and this proves a post-quantum handshake and an
// encrypted stream do, which is what a user actually gets.

package common

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// liveBridgeAddr returns the bridge under test, or skips.
func liveBridgeAddr(t *testing.T) string {
	t.Helper()
	addr := os.Getenv("KD_LIVE_BRIDGE")
	if addr == "" {
		t.Skip("set KD_LIVE_BRIDGE=host:port to run against a deployed bridge")
	}
	return addr
}

// dialLiveLeg opens one leg on the real bridge and sends the bare room token,
// exactly as dialBridgeAddr does for an unfunded peer.
func dialLiveLeg(t *testing.T, addr, ownFP, peerFP, direction string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 15*time.Second)
	require.NoError(t, err, "dial the bridge")
	t.Cleanup(func() { _ = conn.Close() })
	tok := bridgeRoomToken(ownFP, peerFP, direction)
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Write(tok[:])
	require.NoError(t, err, "send the room token")
	require.NoError(t, conn.SetWriteDeadline(time.Time{}))
	return conn
}

// TestLiveBridgeCarriesARealSession runs the real PQC handshake on both legs
// across the deployed bridge and then sends encrypted payload over each.
func TestLiveBridgeCarriesARealSession(t *testing.T) {
	addr := liveBridgeAddr(t)
	logger := testkit.DiscardLogger()

	alice, err := session.InitSession(logger, 26902, 26901)
	require.NoError(t, err)
	bob, err := session.InitSession(logger, 26904, 26903)
	require.NoError(t, err)

	keysOf := func(s *session.Session) *kbc.PeerKeys {
		k, kerr := kbc.ParsePeerKeys(map[string][]byte{
			"x25519": s.OwnKeys.X25519Public.Bytes(),
			"mlkem":  s.OwnKeys.MlKemPublic.Bytes(),
		})
		require.NoError(t, kerr)
		return k
	}
	alice.PeerPubKeys = keysOf(bob)
	bob.PeerPubKeys = keysOf(alice)
	alice.ExpectedPeerFingerprint = bob.OwnFingerprint
	bob.ExpectedPeerFingerprint = alice.OwnFingerprint

	// Both peers derive the same token for a direction, which is the whole
	// basis of the rendezvous: the bridge only ever sees that opaque value.
	require.Equal(t,
		bridgeRoomToken(alice.OwnFingerprint, bob.OwnFingerprint, "pair1"),
		bridgeRoomToken(bob.OwnFingerprint, alice.OwnFingerprint, "pair1"),
		"the two peers must derive one token per direction")

	// pair1 carries alice's inbound and bob's outbound.
	aliceIn := dialLiveLeg(t, addr, alice.OwnFingerprint, bob.OwnFingerprint, "pair1")
	time.Sleep(500 * time.Millisecond) // let the bridge park it
	bobOut := dialLiveLeg(t, addr, bob.OwnFingerprint, alice.OwnFingerprint, "pair1")

	hsErr := make(chan error, 1)
	go func() { hsErr <- session.PerformOutboundHandshakeOnConn(bob, bobOut) }()
	require.NoError(t, session.PerformInboundHandshakeWait(alice, aliceIn, 30*time.Second),
		"alice's inbound handshake across the live bridge")
	select {
	case err := <-hsErr:
		require.NoError(t, err, "bob's outbound handshake across the live bridge")
	case <-time.After(30 * time.Second):
		t.Fatal("bob's outbound handshake never finished")
	}

	// The two sides agreed on a suite and on a key for this direction.
	require.NotEmpty(t, alice.SEKInbound)
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound, "keys must match across the bridge")
	require.Equal(t, bob.CipherSuite, alice.CipherSuite, "one suite, declared and adopted")
	t.Logf("handshake ok across %s, suite %s", addr, alice.CipherSuite)

	// And real ciphertext flows. The bridge forwards it blind.
	bobConn := session.NewSecureConn(bobOut, bob.SEKOutbound, bob.NegotiatedSuite(), session.NoncePrefixOutbound)
	aliceConn := session.NewSecureConn(aliceIn, alice.SEKInbound, alice.NegotiatedSuite(), session.NoncePrefixInbound)

	payload := make([]byte, 512<<10)
	_, err = rand.Read(payload)
	require.NoError(t, err)
	start := time.Now()
	go func() { _, _ = bobConn.Write(payload) }()
	got := make([]byte, len(payload))
	require.NoError(t, aliceConn.SetReadDeadline(time.Now().Add(60*time.Second)))
	_, err = io.ReadFull(aliceConn, got)
	require.NoError(t, err, "read the encrypted payload back")
	require.True(t, bytes.Equal(got, payload), "payload must survive the bridge byte-exact")
	t.Logf("%s of ciphertext byte-exact in %s",
		fmt.Sprintf("%d KiB", len(payload)>>10), time.Since(start).Round(time.Millisecond))
}
