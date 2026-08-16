// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
package discovery

import (
	"log/slog"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
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

	require.Len(t, s.Peers(), 2, "setup")

	s.ClearPeers()

	require.Empty(t, s.Peers(), "after ClearPeers")
}

func TestClearPeers_ServiceKeepsRunning(t *testing.T) {
	s := New(26001, slog.Default())
	require.NoError(t, s.Start(), "start")
	defer s.Stop()

	s.ClearPeers()

	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	require.True(t, running, "service should still be running after ClearPeers")
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

	// Peer disconnects and re-advertises with a new name on a different port.
	// The old entry is still in the map because cleanup has not run yet.
	s.mu.Lock()
	s.peers["192.168.1.10:26005"] = &Peer{
		Name:     "Swift Penguin",
		Addr:     "192.168.1.10:26005",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	// BUG: both entries exist, same IP, different ports, different names.
	peers := s.Peers()
	require.Len(t, peers, 2, "reproduction: expected 2 stale entries")
	t.Logf("BUG REPRODUCED: %d peers from same IP (stale + fresh)", len(peers))
	for _, p := range peers {
		t.Logf("  %s @ %s", p.Name, p.Addr)
	}

	// After ClearPeers and re-discovery, only the fresh one should appear.
	s.ClearPeers()
	if got := len(s.Peers()); got != 0 {
		t.Errorf("after clear: expected 0, got %d", got)
	}

	// Simulate only the new beacon arriving.
	s.mu.Lock()
	s.peers["192.168.1.10:26005"] = &Peer{
		Name:     "Swift Penguin",
		Addr:     "192.168.1.10:26005",
		LastSeen: time.Now(),
	}
	s.mu.Unlock()

	peers = s.Peers()
	require.Len(t, peers, 1, "after re-discovery")
	require.Equal(t, "Swift Penguin", peers[0].Name, "after re-discovery")
}

func TestUpsertPeer_DedupByName(t *testing.T) {
	s := New(26001, slog.Default())

	s.mu.Lock()
	s.upsertPeer("Neon Comet", "192.168.1.10:26003")
	s.mu.Unlock()

	// Same peer re-discovered via mDNS at a different address.
	s.mu.Lock()
	s.upsertPeer("Neon Comet", "192.168.1.10:43210")
	s.mu.Unlock()

	peers := s.Peers()
	require.Len(t, peers, 1, "expected 1 peer after dedup")
	require.Equal(t, "192.168.1.10:43210", peers[0].Addr, "expected new addr")
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
	require.Len(t, peers, 1, "expected 1 peer")
	require.Equal(t, "Fresh Otter", peers[0].Name, "expected fresh name")
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
	require.Len(t, id, 32, "generateInstanceID")
	require.NotEqual(t, id, generateInstanceID(), "two calls returned the same id")
}

func TestServiceBeaconCarriesInstanceID(t *testing.T) {
	s := New(26001, slog.Default())
	b := s.beacon()

	// A second service must advertise a different id so same-named peers differ.
	s2 := New(26001, slog.Default())

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("beacon name", b.Name, s.name),
			fp.Equal("beacon port", b.Port, s.port),
			fp.NotEqual("beacon.ID not empty", b.ID, ""),
			fp.Equal("beacon.ID matches instanceID", b.ID, s.instanceID),
			fp.NotEqual("second service instance id", s2.beacon().ID, b.ID),
		)
	})
}
