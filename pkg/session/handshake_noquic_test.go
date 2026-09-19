// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// OwnNoQUIC: a peer that cannot hold up the QUIC end says so by sending no QUIC
// seeds, which is exactly how an older peer already looks. A browser sets it,
// so a desktop stops re-registering the relayed UDP rooms every 30 s for a
// counterpart that has no UDP at all. The TCP lane is untouched.

package session

import (
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/stretchr/testify/require"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// noQUICPair runs one real handshake with the outbound side's flag set as
// given, and returns both sessions.
func noQUICPair(t *testing.T, outboundNoQUIC bool) (inbound, outbound *Session) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	alice, err := InitSession(logger, 26002, 26001)
	require.NoError(t, err)
	bob, err := InitSession(logger, 26004, 26003)
	require.NoError(t, err)

	alicePeer, err := kbc.ParsePeerKeys(map[string][]byte{
		"x25519": alice.OwnKeys.X25519Public.Bytes(),
		"mlkem":  alice.OwnKeys.MlKemPublic.Bytes(),
	})
	require.NoError(t, err)
	bob.PeerPubKeys = alicePeer
	alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
	require.NoError(t, err)
	bob.OwnNoQUIC = outboundNoQUIC

	bobEnd, aliceEnd := net.Pipe()
	t.Cleanup(func() { _ = bobEnd.Close(); _ = aliceEnd.Close() })
	errCh := make(chan error, 1)
	go func() { errCh <- PerformOutboundHandshakeOnConn(bob, bobEnd) }()
	require.NoError(t, PerformInboundHandshake(alice, aliceEnd))
	require.NoError(t, <-errCh)
	return alice, bob
}

// With the flag set, the flight carries no QUIC seeds and neither side derives
// a QUIC key, so quic_control's "peer did not negotiate a QUIC channel" path is
// what the desktop takes.
func TestOwnNoQUICSuppressesTheQUICSeeds(t *testing.T) {
	alice, bob := noQUICPair(t, true)

	require.Empty(t, bob.SEKOutboundQUIC, "a no-QUIC peer must derive no QUIC key of its own")
	require.Empty(t, alice.SEKInboundQUIC, "and give the far side none to bring a lane up with")

	// The TCP lane is exactly as it was.
	require.NotEmpty(t, bob.SEKOutbound)
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound, "the TCP channel is untouched")
}

// Without the flag, nothing changes: this is the regression guard for every
// desktop-to-desktop session.
func TestWithoutNoQUICTheLaneIsUnchanged(t *testing.T) {
	alice, bob := noQUICPair(t, false)

	require.NotEmpty(t, bob.SEKOutboundQUIC)
	require.Equal(t, bob.SEKOutboundQUIC, alice.SEKInboundQUIC, "the QUIC channel keys still match")
	require.Equal(t, bob.SEKOutbound, alice.SEKInbound)
	require.NotEqual(t, bob.SEKOutbound, bob.SEKOutboundQUIC, "and stay independent of the TCP key")
}
