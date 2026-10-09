// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

func TestQUICLaneIP_FollowsTheFamilyThatAnsweredTheTCPDial(t *testing.T) {
	kd := newBareKD()
	kd.PeerIPv6IP = "2001:db8::2"
	kd.PeerIPv4IP = "203.0.113.9"
	require.Equal(t, "2001:db8::2", kd.quicLaneIP(), "no direct dial yet: the advertised address, as before")

	// The IPv6 dial found no route and the IPv4 dial reached the peer.
	kd.peerDialedIP.Store("203.0.113.9")
	require.Equal(t, "203.0.113.9", kd.quicLaneIP(), "the lane follows the family that answered")

	kd.peerDialedIP.Store("2001:db8::2")
	require.Equal(t, "2001:db8::2", kd.quicLaneIP())

	kd.peerDialedIP.Store("")
	kd.PeerIPv6IP = ""
	require.Equal(t, "", kd.quicLaneIP(), "bridge session, no advertised address: no lane dial, as before")
}

// A peer up to 0.4.8 with no global IPv6 advertises ::1 or fe80::. Neither is
// dialed: ::1 reaches this host and fe80:: without a zone reaches none (BUGS 35).
func TestDialablePeerIPv6(t *testing.T) {
	for _, tc := range []struct {
		peer, own string
		want      bool
	}{
		{"2001:db8::2", "", true},
		{"fd7a:115c:a1e0::1", "", true}, // ULA, a tailnet for one: dialed as before
		{"fe80::8929:1796:58be:8feb", "", false},
		{"fe80::8929:1796:58be:8feb", "2001:db8::1", false},
		{"::1", "", false},
		{"::1", "2001:db8::1", false},
		{"::1", "::1", true}, // the tests run both peers on loopback
		{"203.0.113.9", "", false},
		{"", "", false},
		{"not-an-ip", "", false},
	} {
		kd := newBareKD()
		kd.LocalIPv6IP = tc.own
		require.Equal(t, tc.want, kd.dialablePeerIPv6(tc.peer), "peer %q, own %q", tc.peer, tc.own)
	}
}

// The joiner takes the creator's IPv6 from the relay only when it can dial it,
// so it skips an old peer's ::1 or fe80:: instead of dialing it first.
func TestGetRoomFromRelay_SkipsAnIPv6ThisHostCannotDial(t *testing.T) {
	var mu sync.Mutex
	store := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/register":
			body, _ := io.ReadAll(r.Body)
			store[token] = body
			w.WriteHeader(http.StatusCreated)
		case "/fetch":
			if b, ok := store[token]; ok {
				_, _ = w.Write(b)
				return
			}
			http.NotFound(w, r)
		default:
			http.NotFound(w, r) // No /probe, like an old relay.
		}
	}))
	t.Cleanup(srv.Close)
	relay, err := url.Parse(srv.URL)
	require.NoError(t, err)

	engine := func(ipv6 string) *KeibiDrop {
		t.Helper()
		port := pickFreePortPair(t)
		kd, err := NewKeibiDropWithIP(t.Context(), roundTestLogger(), false, relay, port, port+1, "", t.TempDir(), false, false, ipv6)
		require.NoError(t, err)
		t.Cleanup(func() { _ = kd.listener.Close() })
		return kd
	}
	for _, tc := range []struct{ creator, joiner, want string }{
		{"2001:db8::1", "", "2001:db8::1"},
		{"fe80::8929:1796:58be:8feb", "", ""}, // a 0.4.8 Windows peer
		{"::1", "", ""},                       // a 0.4.8 Mac or Linux peer
		{"::1", "::1", "::1"},                 // both peers on this host, as in the tests
	} {
		creator, joiner := engine(tc.creator), engine(tc.joiner)
		require.NoError(t, creator.registerRoomToRelay())
		require.NoError(t, joiner.getRoomFromRelay(creator.session.OwnFingerprint))
		require.Equal(t, tc.want, joiner.PeerIPv6IP, "creator advertises %q, joiner %q", tc.creator, tc.joiner)
	}
}
