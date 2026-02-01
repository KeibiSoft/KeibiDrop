// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// ReconnectState represents the current state of the reconnection manager.
type ReconnectState int32

const (
	ReconnectStateConnected    ReconnectState = iota // Connection is healthy
	ReconnectStateReconnecting                       // Actively trying to reconnect
	ReconnectStateWaitingPeer                        // Waiting for peer to come online
	ReconnectStateGaveUp                             // Exhausted all retry attempts
)

// ReconnectManager handles automatic reconnection when the P2P connection drops.
// It coordinates with the health monitor and uses a deterministic initiator
// selection to avoid race conditions.
type ReconnectManager struct {
	session  *Session
	logger   *slog.Logger

	// State
	state    atomic.Int32 // ReconnectState
	attempts atomic.Int32
	mu       sync.Mutex

	// Configuration
	Backoff     []time.Duration // Exponential backoff delays
	MaxAttempts int             // Maximum reconnection attempts

	// Connection details (cached from last successful connection)
	CachedPeerIP   string
	CachedPeerPort int

	// Callbacks
	OnReconnecting  func()                               // Called when reconnection starts
	OnReconnected   func()                               // Called on successful reconnection
	OnGaveUp        func()                               // Called when all attempts exhausted
	RelayRefresh    func() error                         // Re-register with relay
	RelayLookup     func(fingerprint string) (ip string, port int, err error) // Lookup peer in relay
	AcceptConn      func(timeout time.Duration) (net.Conn, error)             // Accept incoming connection

	// Control
	ctx    context.Context
	cancel context.CancelFunc
}

// NewReconnectManager creates a new reconnection manager with default settings.
func NewReconnectManager(session *Session, logger *slog.Logger) *ReconnectManager {
	return &ReconnectManager{
		session: session,
		logger:  logger.With("component", "reconnect-manager"),
		Backoff: []time.Duration{
			1 * time.Second,
			2 * time.Second,
			4 * time.Second,
			8 * time.Second,
			16 * time.Second,
			30 * time.Second,
		},
		MaxAttempts: 10,
	}
}

// State returns the current reconnection state.
func (r *ReconnectManager) State() ReconnectState {
	return ReconnectState(r.state.Load())
}

// Attempts returns the number of reconnection attempts made.
func (r *ReconnectManager) Attempts() int {
	return int(r.attempts.Load())
}

// IsReconnectInitiator determines which peer should initiate reconnection.
// The peer with the lexicographically lower fingerprint is the initiator.
// This avoids race conditions where both peers try to connect simultaneously.
func (r *ReconnectManager) IsReconnectInitiator() bool {
	if r.session == nil {
		return false
	}
	return r.session.OwnFingerprint < r.session.ExpectedPeerFingerprint
}

// OnDisconnect is called when the health monitor detects a connection loss.
// It starts the reconnection loop in a goroutine.
func (r *ReconnectManager) OnDisconnect() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ReconnectState(r.state.Load()) == ReconnectStateReconnecting {
		return // Already reconnecting
	}

	r.state.Store(int32(ReconnectStateReconnecting))
	r.attempts.Store(0)

	if r.OnReconnecting != nil {
		r.OnReconnecting()
	}

	r.ctx, r.cancel = context.WithCancel(context.Background())
	go r.reconnectLoop()
}

// Stop halts any ongoing reconnection attempts.
func (r *ReconnectManager) Stop() {
	if r.cancel != nil {
		r.cancel()
	}
}

// Reset resets the manager to connected state (call after manual session restart).
func (r *ReconnectManager) Reset() {
	r.state.Store(int32(ReconnectStateConnected))
	r.attempts.Store(0)
}

func (r *ReconnectManager) reconnectLoop() {
	logger := r.logger.With("initiator", r.IsReconnectInitiator())

	for {
		select {
		case <-r.ctx.Done():
			logger.Info("Reconnection cancelled")
			return
		default:
		}

		attempt := int(r.attempts.Add(1))
		if attempt > r.MaxAttempts {
			r.state.Store(int32(ReconnectStateGaveUp))
			logger.Error("Gave up reconnecting", "attempts", attempt-1)
			if r.OnGaveUp != nil {
				r.OnGaveUp()
			}
			return
		}

		// Calculate backoff delay
		backoffIdx := attempt - 1
		if backoffIdx >= len(r.Backoff) {
			backoffIdx = len(r.Backoff) - 1
		}
		delay := r.Backoff[backoffIdx]

		logger.Info("Reconnection attempt",
			"attempt", attempt,
			"maxAttempts", r.MaxAttempts,
			"delay", delay)

		// Wait before attempting
		select {
		case <-r.ctx.Done():
			return
		case <-time.After(delay):
		}

		if err := r.attemptReconnect(); err == nil {
			r.state.Store(int32(ReconnectStateConnected))
			r.attempts.Store(0)
			logger.Info("Reconnection successful")

			if r.OnReconnected != nil {
				r.OnReconnected()
			}
			return
		} else {
			logger.Warn("Reconnection attempt failed", "error", err)
		}
	}
}

func (r *ReconnectManager) attemptReconnect() error {
	logger := r.logger.With("phase", "reconnect-attempt")

	// Step 1: Re-register with relay (in case our registration expired)
	if r.RelayRefresh != nil {
		if err := r.RelayRefresh(); err != nil {
			logger.Warn("Relay re-registration failed", "error", err)
			// Continue anyway - peer might connect directly using cached IP
		}
	}

	// Step 2: Execute role-specific reconnection
	var err error
	if r.IsReconnectInitiator() {
		// Wait 1 second to let responder start listening
		time.Sleep(1 * time.Second)
		err = r.reconnectAsInitiator()
	} else {
		err = r.reconnectAsResponder()
	}

	if err != nil {
		return err
	}

	// Step 3: Verify the new connection
	if r.session.Session == nil {
		return fmt.Errorf("session sockets not initialized after handshake")
	}

	return nil
}

func (r *ReconnectManager) reconnectAsInitiator() error {
	logger := r.logger.With("role", "initiator")

	// Try cached IP first (faster if peer didn't change IP)
	if r.CachedPeerIP != "" && r.CachedPeerPort > 0 {
		addr := net.JoinHostPort(r.CachedPeerIP, fmt.Sprintf("%d", r.CachedPeerPort))
		logger.Debug("Trying cached address", "addr", addr)

		if err := PerformOutboundHandshake(r.session, addr); err == nil {
			logger.Info("Reconnected using cached address")
			return nil
		}
		logger.Debug("Cached address failed, trying relay lookup")
	}

	// Fall back to relay lookup
	if r.RelayLookup == nil {
		return fmt.Errorf("no relay lookup function configured")
	}

	ip, port, err := r.RelayLookup(r.session.ExpectedPeerFingerprint)
	if err != nil {
		return fmt.Errorf("relay lookup failed: %w", err)
	}

	// Update cache
	r.CachedPeerIP = ip
	r.CachedPeerPort = port

	addr := net.JoinHostPort(ip, fmt.Sprintf("%d", port))
	logger.Debug("Connecting via relay lookup", "addr", addr)

	if err := PerformOutboundHandshake(r.session, addr); err != nil {
		return fmt.Errorf("outbound handshake failed: %w", err)
	}

	return nil
}

func (r *ReconnectManager) reconnectAsResponder() error {
	logger := r.logger.With("role", "responder")

	if r.AcceptConn == nil {
		return fmt.Errorf("no accept function configured")
	}

	// Accept connection with timeout
	timeout := 30 * time.Second
	logger.Debug("Waiting for initiator connection", "timeout", timeout)

	conn, err := r.AcceptConn(timeout)
	if err != nil {
		return fmt.Errorf("accept failed: %w", err)
	}

	logger.Info("Accepted reconnection", "remote", conn.RemoteAddr())

	if err := PerformInboundHandshake(r.session, conn); err != nil {
		conn.Close()
		return fmt.Errorf("inbound handshake failed: %w", err)
	}

	return nil
}

// String returns a human-readable representation of the reconnect state.
func (s ReconnectState) String() string {
	switch s {
	case ReconnectStateConnected:
		return "connected"
	case ReconnectStateReconnecting:
		return "reconnecting"
	case ReconnectStateWaitingPeer:
		return "waiting_for_peer"
	case ReconnectStateGaveUp:
		return "gave_up"
	default:
		return "unknown"
	}
}
