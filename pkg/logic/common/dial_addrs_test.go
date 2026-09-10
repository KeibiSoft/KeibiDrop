// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPeerDialAddrs_IPv6ThenIPv4_SkippingBlockedFamilies(t *testing.T) {
	kd := newBareKD()
	kd.PeerIPv6IP = "2001:db8::2"
	kd.PeerIPv4IP = "203.0.113.9"
	require.Equal(t, []string{"[2001:db8::2]:26001", "203.0.113.9:26001"}, kd.peerDialAddrs(26001))

	kd.peerInboundBlocked.Store(true)
	require.Equal(t, []string{"203.0.113.9:26001"}, kd.peerDialAddrs(26001), "a blocked IPv6 inbound is not dialed")
	kd.peerInbound4Blocked.Store(true)
	require.Empty(t, kd.peerDialAddrs(26001), "both blocked: nothing to dial")

	kd.peerInboundBlocked.Store(false)
	kd.peerInbound4Blocked.Store(false)
	kd.PeerIPv6IP = ""
	require.Equal(t, []string{"203.0.113.9:26001"}, kd.peerDialAddrs(26001), "an IPv4-only peer is dialed")
	kd.PeerIPv4IP = ""
	require.Empty(t, kd.peerDialAddrs(26001))
}

func TestPeerDialAddrs_PreferIPv4Switch(t *testing.T) {
	orig := preferIPv4
	preferIPv4 = true
	defer func() { preferIPv4 = orig }()
	kd := newBareKD()
	kd.PeerIPv6IP = "2001:db8::2"
	kd.PeerIPv4IP = "203.0.113.9"
	require.Equal(t, []string{"203.0.113.9:26001", "[2001:db8::2]:26001"}, kd.peerDialAddrs(26001))
}

func TestDialPeerDirect_NothingToDialIsAnError(t *testing.T) {
	kd := newBareKD()
	require.ErrorIs(t, kd.dialPeerDirect(kd.logger, nil), errNoDirectAddress)
}

func TestPeerDirectIP_FollowsTheFamilyThatAnswered(t *testing.T) {
	kd := newBareKD()
	kd.PeerIPv6IP = "2001:db8::2"
	kd.PeerIPv4IP = "203.0.113.9"
	require.Equal(t, "2001:db8::2", kd.peerDirectIP(), "before any dial the IPv6 address stands")

	kd.peerDialedIP.Store("203.0.113.9")
	kd.PeerIPv4IP = "198.51.100.4" // the peer re-registered on a new IPv4
	require.Equal(t, "198.51.100.4", kd.peerDirectIP(), "the family that answered, at its current address")

	kd.peerDialedIP.Store("2001:db8::2")
	require.Equal(t, "2001:db8::2", kd.peerDirectIP())

	kd.PeerIPv6IP, kd.PeerIPv4IP = "", ""
	require.Equal(t, "2001:db8::2", kd.peerDirectIP(), "nothing advertised: the address that answered")
	kd.peerDialedIP.Store("")
	kd.PeerIPv4IP = "198.51.100.4"
	require.Equal(t, "198.51.100.4", kd.peerDirectIP(), "IPv4 is the only address known")
}

func TestListen4Hint_CarriesTheRelayVerdict(t *testing.T) {
	kd := newBareKD()
	kd.inboundPort = 26001
	require.Nil(t, kd.listen4Hint(), "no IPv4 known before a probe answered")
	kd.publicIPv4.Store("203.0.113.7")
	kd.inbound4Reachable.Store(true)
	h := kd.listen4Hint()
	require.Equal(t, &ConnectionHint{IP: "203.0.113.7", Proto: "tcp", Port: 26001}, h)
	kd.inbound4Reachable.Store(false)
	require.True(t, kd.listen4Hint().InboundBlocked)
}
