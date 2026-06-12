// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: Tests the local-connect role decider gives two peers opposite create/join
// ABOUTME: roles, including the equal-name collision where the address breaks the tie.

package common

import (
	"net"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

func TestLocalConnectRole(t *testing.T) {
	// Smaller name creates (the plain name tiebreak, unchanged).
	if !LocalConnectRole("alice", "bob", "", "") {
		t.Error("alice < bob should create")
	}
	if LocalConnectRole("bob", "alice", "", "") {
		t.Error("bob > alice should join")
	}

	// Mirrored inputs (the two peers) must always pick opposite roles.
	pairs := []struct{ n1, n2, a1, a2 string }{
		{"alice", "bob", "::1", "::2"},     // names differ
		{"same", "same", "::1", "::2"},     // names collide -> address breaks tie
		{"x", "x", "10.0.0.1", "10.0.0.2"}, // names collide, IPv4
	}
	for _, p := range pairs {
		peer1 := LocalConnectRole(p.n1, p.n2, p.a1, p.a2)
		peer2 := LocalConnectRole(p.n2, p.n1, p.a2, p.a1)
		if peer1 == peer2 {
			t.Errorf("both peers picked create=%v for %+v", peer1, p)
		}
	}

	// Equal names: the smaller address creates.
	if !LocalConnectRole("x", "x", "::1", "::2") {
		t.Error("equal names: smaller address should create")
	}

	// Full tie (names and addresses equal): unresolved, both sides pick join.
	// Both simulated sides are the same call here since mirroring equal args
	// changes nothing; documents the residual limitation DecideLocalRole warns about.
	if LocalConnectRole("x", "x", "10.0.0.1", "10.0.0.1") {
		t.Error("full tie: expected join (create=false) on both sides")
	}
}

func TestGetDiscoveryLANAddress(t *testing.T) {
	addr := GetDiscoveryLANAddress()
	if addr == "" {
		return // a host with no private IPv4 is acceptable
	}
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil {
		t.Errorf("expected a bare IPv4, got %q", addr)
	}
	if ip != nil && !ip.IsPrivate() {
		t.Errorf("expected a private address, got %q", addr)
	}
}

func TestDecideLocalRole(t *testing.T) {
	// Distinct names: the name decides for a normal LAN (RFC1918) peer.
	if !DecideLocalRole("alice", "bob", "192.168.1.9:26431") {
		t.Error("alice < bob should create")
	}
	if DecideLocalRole("bob", "alice", "192.168.1.9:26431") {
		t.Error("bob > alice should join")
	}
	// The peer port is stripped, so "ip:port" and bare "ip" decide the same.
	if DecideLocalRole("x", "x", "10.0.0.5:26431") != DecideLocalRole("x", "x", "10.0.0.5") {
		t.Error("peer port must not change the decision")
	}
	// The zone is stripped like the port, so every stored form of the same
	// peer IP decides identically, whatever the zone or bracketing.
	base := DecideLocalRole("x", "x", "fe80::2%eth0")
	for _, form := range []string{"fe80::2%eth0:26431", "[fe80::2%eth0]:26431", "fe80::2%wlan1"} {
		if DecideLocalRole("x", "x", form) != base {
			t.Errorf("address form %q changed the decision", form)
		}
	}
	// And it matches feeding LocalConnectRole our own LAN IPv4 + the stripped peer IP.
	if DecideLocalRole("x", "x", "10.0.0.5:26431") != LocalConnectRole("x", "x", GetDiscoveryLANAddress(), "10.0.0.5") {
		t.Error("DecideLocalRole should equal LocalConnectRole on the stripped peer IP")
	}
	// Names equal and the compared addresses equal (two daemons on one host):
	// the tie stays unresolved and this side deterministically picks join.
	if mine := GetDiscoveryLANAddress(); mine != "" {
		if DecideLocalRole("x", "x", mine) {
			t.Error("full tie: expected join (create=false)")
		}
	}
}

func TestSetPeerDirectAddress(t *testing.T) {
	cases := []struct {
		name     string
		addr     string
		wantIP   string
		wantPort int
		wantErr  bool
	}{
		{"discovery ipv4", "192.168.1.5:26431", "192.168.1.5", 26431, false},
		{"link-local with zone", "fe80::1%eth0:26431", "fe80::1%eth0", 26431, false},
		{"bare ipv4 lacks port", "192.168.1.5", "", 0, true},
		{"zoned form lacks port", "fe80::1%eth0", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kd := &KeibiDrop{session: &session.Session{}}
			err := kd.SetPeerDirectAddress(c.addr)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", c.addr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.addr, err)
			}
			if kd.PeerIPv6IP != c.wantIP || kd.session.PeerPort != c.wantPort {
				t.Errorf("got ip=%q port=%d, want ip=%q port=%d",
					kd.PeerIPv6IP, kd.session.PeerPort, c.wantIP, c.wantPort)
			}
		})
	}
}
