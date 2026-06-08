// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: Tests the local-connect role decider gives two peers opposite create/join
// ABOUTME: roles, including the equal-name collision where the address breaks the tie.

package common

import (
	"net"
	"testing"
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
	// Distinct names: the name decides and the peer address form is irrelevant.
	if !DecideLocalRole("alice", "bob", "203.0.113.9:26431") {
		t.Error("alice < bob should create")
	}
	if DecideLocalRole("bob", "alice", "203.0.113.9:26431") {
		t.Error("bob > alice should join")
	}
	// The peer port is stripped, so "ip:port" and bare "ip" decide the same.
	if DecideLocalRole("x", "x", "10.0.0.5:26431") != DecideLocalRole("x", "x", "10.0.0.5") {
		t.Error("peer port must not change the decision")
	}
	// And it matches feeding LocalConnectRole our own LAN IPv4 + the stripped peer IP.
	if DecideLocalRole("x", "x", "10.0.0.5:26431") != LocalConnectRole("x", "x", GetDiscoveryLANAddress(), "10.0.0.5") {
		t.Error("DecideLocalRole should equal LocalConnectRole on the stripped peer IP")
	}
}
