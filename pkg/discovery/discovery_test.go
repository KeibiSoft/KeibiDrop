// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
package discovery

import (
	"log/slog"
	"testing"
	"time"
)

func TestClearPeers_EmptiesMap(t *testing.T) {
	s := New(26001, slog.Default())
	s.mu.Lock()
	s.peers["192.168.1.10:26003"] = &Peer{
		Name:     "Stale Tiger",
		Addr:     "192.168.1.10:26003",
		LastSeen: time.Now(),
	}
	s.peers["192.168.1.11:26004"] = &Peer{
		Name:     "Old Falcon",
		Addr:     "192.168.1.11:26004",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	if len(s.Peers()) != 2 {
		t.Fatalf("setup: expected 2 peers, got %d", len(s.Peers()))
	}

	s.ClearPeers()

	if got := len(s.Peers()); got != 0 {
		t.Errorf("after ClearPeers: expected 0 peers, got %d", got)
	}
}

func TestClearPeers_ServiceKeepsRunning(t *testing.T) {
	s := New(26001, slog.Default())
	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	s.ClearPeers()

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if !running {
		t.Error("service should still be running after ClearPeers")
	}
}

func TestStalePeers_ReproduceBug(t *testing.T) {
	s := New(26001, slog.Default())

	s.mu.Lock()
	s.peers["192.168.1.10:26003"] = &Peer{
		Name:     "Cosmic Waffle",
		Addr:     "192.168.1.10:26003",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	// Peer disconnects and re-advertises with a new name on a DIFFERENT port.
	// The old entry is still in the map because cleanup hasn't run yet.
	s.mu.Lock()
	s.peers["192.168.1.10:26005"] = &Peer{
		Name:     "Swift Penguin",
		Addr:     "192.168.1.10:26005",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	// BUG: both entries exist — same IP, different ports, different names.
	peers := s.Peers()
	if len(peers) != 2 {
		t.Fatalf("reproduction: expected 2 stale entries, got %d", len(peers))
	}
	t.Logf("BUG REPRODUCED: %d peers from same IP (stale + fresh)", len(peers))
	for _, p := range peers {
		t.Logf("  %s @ %s", p.Name, p.Addr)
	}

	// After ClearPeers + re-discovery, only the fresh one should appear.
	s.ClearPeers()
	if got := len(s.Peers()); got != 0 {
		t.Errorf("after clear: expected 0, got %d", got)
	}

	// Simulate only the new beacon arriving
	s.mu.Lock()
	s.peers["192.168.1.10:26005"] = &Peer{
		Name:     "Swift Penguin",
		Addr:     "192.168.1.10:26005",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	peers = s.Peers()
	if len(peers) != 1 {
		t.Errorf("after re-discovery: expected 1 peer, got %d", len(peers))
	}
	if len(peers) == 1 && peers[0].Name != "Swift Penguin" {
		t.Errorf("expected 'Swift Penguin', got %q", peers[0].Name)
	}
}

func TestUpsertPeer_DedupByName(t *testing.T) {
	s := New(26001, slog.Default())

	s.mu.Lock()
	s.upsertPeer("Neon Comet", "192.168.1.10:26003")
	s.mu.Unlock()

	// Same peer re-discovered via mDNS at a different address
	s.mu.Lock()
	s.upsertPeer("Neon Comet", "192.168.1.10:43210")
	s.mu.Unlock()

	peers := s.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer after dedup, got %d", len(peers))
	}
	if peers[0].Addr != "192.168.1.10:43210" {
		t.Errorf("expected new addr 192.168.1.10:43210, got %s", peers[0].Addr)
	}
}

func TestClearPeers_FreshBeaconsAppear(t *testing.T) {
	s := New(26001, slog.Default())
	s.mu.Lock()
	s.peers["192.168.1.10:26003"] = &Peer{
		Name:     "Stale Tiger",
		Addr:     "192.168.1.10:26003",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	s.ClearPeers()

	s.mu.Lock()
	s.peers["192.168.1.10:26003"] = &Peer{
		Name:     "Fresh Otter",
		Addr:     "192.168.1.10:26003",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	peers := s.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	if peers[0].Name != "Fresh Otter" {
		t.Errorf("expected name 'Fresh Otter', got %q", peers[0].Name)
	}
}

func TestIsSelfBeacon(t *testing.T) {
	const myID = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6"
	cases := []struct {
		desc string
		b    Beacon
		want bool
	}{
		{"own beacon, same id", Beacon{Name: "Cosmic Waffle", ID: myID}, true},
		// Same display name but a different id is a real peer, not self. This is
		// the collision the name-only filter used to drop.
		{"same name, different id", Beacon{Name: "Cosmic Waffle", ID: "ffffffffffffffffffffffffffffffff"}, false},
		{"legacy peer, no id", Beacon{Name: "Cosmic Waffle", ID: ""}, false},
		{"different name and id", Beacon{Name: "Swift Penguin", ID: "ffffffffffffffffffffffffffffffff"}, false},
	}
	for _, c := range cases {
		if got := isSelfBeacon(c.b, myID); got != c.want {
			t.Errorf("%s: isSelfBeacon = %v, want %v", c.desc, got, c.want)
		}
	}
}

func TestGenerateInstanceID(t *testing.T) {
	id := generateInstanceID()
	if len(id) != 32 {
		t.Errorf("len(id) = %d, want 32 hex chars", len(id))
	}
	if id == generateInstanceID() {
		t.Error("two calls returned the same id")
	}
}

func TestServiceBeaconCarriesInstanceID(t *testing.T) {
	s := New(26001, slog.Default())
	b := s.beacon()
	if b.Name != s.name || b.Port != s.port {
		t.Errorf("beacon = %+v, want name=%q port=%d", b, s.name, s.port)
	}
	if b.ID == "" || b.ID != s.instanceID {
		t.Errorf("beacon.ID = %q, want service instanceID %q", b.ID, s.instanceID)
	}
	// A second service must advertise a different id so same-named peers differ.
	if s2 := New(26001, slog.Default()); s2.beacon().ID == b.ID {
		t.Error("two services share an instance id")
	}
}
