// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// ABOUTME: Tests LocalConnectRole gives two peers opposite create/join roles,
// ABOUTME: including the equal-name collision where the address breaks the tie.

package common

import "testing"

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
