// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// Package discovery implements LAN peer discovery using UDP multicast.
// No third-party dependencies — uses only net and encoding/json from stdlib.
//
// Peers broadcast a small JSON beacon on a multicast group every few seconds.
// Each beacon contains a random two-word name (rotated per session) and the
// listening port. No fingerprints or identity info is broadcast.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	multicastAddr  = "224.0.0.167:26999" // KeibiDrop discovery group
	beaconInterval = 3 * time.Second
	peerTimeout    = 10 * time.Second
	maxBeaconSize  = 512
)

// Beacon is the UDP payload broadcast by each peer.
type Beacon struct {
	Name string `json:"n"`           // random two-word name (e.g., "Cosmic Waffle")
	Port int    `json:"p"`           // TCP listening port for KeibiDrop
	Ver  int    `json:"v,omitempty"` // protocol version (1)
}

// Peer represents a discovered peer on the LAN.
type Peer struct {
	Name     string
	Addr     string // "192.168.1.42:26431"
	LastSeen time.Time
}

// Service handles both advertising and browsing for KeibiDrop peers.
type Service struct {
	name   string
	port   int
	logger *slog.Logger

	mu      sync.RWMutex
	peers   map[string]*Peer // keyed by addr
	cancel  context.CancelFunc
	running bool
}

// New creates a discovery service with a random two-word name.
func New(port int, logger *slog.Logger) *Service {
	return &Service{
		name:   generateName(),
		port:   port,
		logger: logger.With("component", "discovery"),
		peers:  make(map[string]*Peer),
	}
}

// Start begins advertising this peer and listening for others.
func (s *Service) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.running = true
	s.mu.Unlock()

	go s.advertise(ctx)
	go s.listen(ctx)
	go s.cleanup(ctx)

	s.logger.Info("Discovery started", "name", s.name, "port", s.port)
	return nil
}

// Stop stops advertising and listening.
func (s *Service) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running {
		return
	}
	s.cancel()
	s.running = false
	s.peers = make(map[string]*Peer)
	s.logger.Info("Discovery stopped")
}

// Name returns this peer's random display name.
func (s *Service) Name() string {
	return s.name
}

// Peers returns a snapshot of currently discovered peers.
func (s *Service) Peers() []Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		result = append(result, *p)
	}
	return result
}

func (s *Service) advertise(ctx context.Context) {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		s.logger.Error("Failed to resolve multicast addr", "error", err)
		return
	}

	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		s.logger.Error("Failed to dial multicast", "error", err)
		return
	}
	defer conn.Close()

	beacon := Beacon{Name: s.name, Port: s.port, Ver: 1}
	data, _ := json.Marshal(beacon)

	ticker := time.NewTicker(beaconInterval)
	defer ticker.Stop()

	// Send immediately, then on tick.
	_, _ = conn.Write(data)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = conn.Write(data)
		}
	}
}

func (s *Service) listen(ctx context.Context) {
	addr, err := net.ResolveUDPAddr("udp4", multicastAddr)
	if err != nil {
		s.logger.Error("Failed to resolve multicast addr", "error", err)
		return
	}

	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		s.logger.Error("Failed to listen multicast", "error", err)
		return
	}
	defer conn.Close()

	_ = conn.SetReadBuffer(maxBeaconSize * 10)

	buf := make([]byte, maxBeaconSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			continue
		}

		var beacon Beacon
		if err := json.Unmarshal(buf[:n], &beacon); err != nil {
			continue
		}

		// Skip our own beacons.
		if beacon.Name == s.name {
			continue
		}

		peerAddr := fmt.Sprintf("%s:%d", src.IP.String(), beacon.Port)

		s.mu.Lock()
		s.peers[peerAddr] = &Peer{
			Name:     beacon.Name,
			Addr:     peerAddr,
			LastSeen: time.Now(),
		}
		s.mu.Unlock()
	}
}

func (s *Service) cleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for addr, p := range s.peers {
				if now.Sub(p.LastSeen) > peerTimeout {
					delete(s.peers, addr)
				}
			}
			s.mu.Unlock()
		}
	}
}
