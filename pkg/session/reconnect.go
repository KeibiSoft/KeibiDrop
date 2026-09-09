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

// ReconnectManager handles automatic reconnection when the P2P connection drops. It coordinates
// with the health monitor and uses deterministic initiator selection to avoid race conditions.
type ReconnectManager struct {
	session *Session
	logger  *slog.Logger

	// State
	state            atomic.Int32 // ReconnectState
	attempts         atomic.Int32
	fallbackToBridge atomic.Bool // Direct failed this outage; later attempts go bridge-first.
	mu               sync.Mutex

	// Configuration
	Backoff     []time.Duration // Exponential backoff delays
	MaxAttempts int             // Maximum reconnection attempts

	// Connection details (cached from last successful connection)
	CachedPeerIP   string
	CachedPeerPort int

	// Bridge relay (for firewall traversal). The fallback transport; PreferDirect orders the attempts.
	BridgeAddr string
	DialBridge func(direction string) (net.Conn, error) // Dial bridge with direction-tagged room token

	// PreferDirect reports whether this session should retry the direct path before
	// the bridge. Nil means bridge-first whenever a bridge is configured.
	PreferDirect func() bool

	// MixedLegs reports a session made with the joiner's outbound leg direct and the
	// creator's outbound leg on the bridge (pair2). Such a session reconnects the same
	// way first; a failure flips the outage to bridge-first like a failed direct attempt.
	MixedLegs func() bool

	// lastTransport is the transport of the last successful reconnect: "direct",
	// "bridge" or "mixed". The engine reads it to keep its connection mode truthful.
	lastTransport atomic.Value

	// Callbacks
	OnReconnecting func()                                                    // Called when reconnection starts
	OnReconnected  func()                                                    // Called on successful reconnection
	OnGaveUp       func()                                                    // Called when all attempts exhausted
	RelayRefresh   func() error                                              // Re-register with relay
	RelayLookup    func(fingerprint string) (ip string, port int, err error) // Lookup peer in relay
	AcceptConn     func(timeout time.Duration) (net.Conn, error)             // Accept incoming connection

	// Control
	ctx     context.Context
	cancel  context.CancelFunc
	stopped atomic.Bool
	done    chan struct{}
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

// IsReconnectInitiator determines which peer should initiate reconnection: the peer with the
// lexicographically lower fingerprint, so both peers do not connect simultaneously.
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

	if r.stopped.Load() {
		return // Manager has been permanently stopped
	}

	if ReconnectState(r.state.Load()) == ReconnectStateReconnecting {
		return // Already reconnecting
	}

	r.state.Store(int32(ReconnectStateReconnecting))
	r.attempts.Store(0)
	r.fallbackToBridge.Store(false)

	if r.OnReconnecting != nil {
		r.OnReconnecting()
	}

	r.ctx, r.cancel = context.WithCancel(context.Background())
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		r.reconnectLoop()
	}()
}

// Stop halts any ongoing reconnection attempts and prevents new ones.
func (r *ReconnectManager) Stop() {
	r.stopped.Store(true)
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if r.done != nil {
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
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

		if r.stopped.Load() {
			return
		}
		if err := r.attemptReconnect(); err == nil {
			r.state.Store(int32(ReconnectStateConnected))
			r.attempts.Store(0)
			logger.Info("Reconnection successful")

			// Arm the ratchet on the fresh conns before OnReconnected rebuilds the gRPC
			// transport: both handshakes are done so the capability is known, and no reader
			// goroutine touches these conns yet.
			r.session.ApplyKeyUpdateNegotiation()

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

	if r.useBridgeFirst() {
		return r.finishTransport(transportBridge, r.reconnectBridge(logger, true))
	}
	if r.mixedFirst() {
		return r.finishTransport(transportMixed, r.noteDirectFailure(logger, r.reconnectMixed(logger, true)))
	}
	return r.finishTransport(transportDirect, r.noteDirectFailure(logger, r.reconnectDirectInitiator(logger)))
}

// noteDirectFailure flips the outage to bridge-first after a failed direct or mixed
// attempt. Direct and bridge stay in separate attempts, so a half-done direct
// handshake never leaks into the bridge pairing.
func (r *ReconnectManager) noteDirectFailure(logger *slog.Logger, err error) error {
	if err != nil && r.BridgeAddr != "" && r.DialBridge != nil {
		r.fallbackToBridge.Store(true)
		logger.Info("Direct reconnect failed, next attempt uses the bridge", "error", err)
	}
	return err
}

// finishTransport records which transport a successful attempt used.
func (r *ReconnectManager) finishTransport(transport string, err error) error {
	if err == nil {
		r.lastTransport.Store(transport)
	}
	return err
}

// LastTransport is the transport of the last successful reconnect, or "" before one.
func (r *ReconnectManager) LastTransport() string {
	v, _ := r.lastTransport.Load().(string)
	return v
}

// useBridgeFirst orders the transports for this attempt.
func (r *ReconnectManager) useBridgeFirst() bool {
	if r.BridgeAddr == "" || r.DialBridge == nil {
		return false
	}
	if r.fallbackToBridge.Load() {
		return true
	}
	if r.mixedFirst() {
		return false // The mixed shape is tried before the bridge, see reconnectMixed.
	}
	return r.PreferDirect == nil || !r.PreferDirect()
}

// mixedFirst reports that this attempt should rebuild the mixed shape: the session was
// made that way, the bridge is there for the relayed leg, and no attempt has failed yet.
func (r *ReconnectManager) mixedFirst() bool {
	return r.MixedLegs != nil && r.MixedLegs() && r.BridgeAddr != "" && r.DialBridge != nil && !r.fallbackToBridge.Load()
}

// reconnectBridge redoes both directions via the bridge, with the same room
// directions as a fresh create and join: the lower fingerprint (creator there,
// initiator here) reads pair1 as its inbound and writes pair2 as its outbound;
// the other side the reverse. The mapping has to match across the two paths,
// because a peer that restarted mid-outage comes back through a fresh create or
// join while this side is still reconnecting. Measured with the NAS container:
// with the roles inverted both sides read pair1 and every attempt timed out.
func (r *ReconnectManager) reconnectBridge(logger *slog.Logger, initiator bool) error {
	logger.Info("Reconnecting via bridge", "addr", r.BridgeAddr)

	if initiator {
		inConn, err := r.DialBridge("pair1")
		if err != nil {
			return fmt.Errorf("bridge dial (inbound): %w", err)
		}
		// Bridge leg: the peer arrives on its own reconnect cadence, so give the
		// first byte the full handshake bound instead of the accept-site default.
		if err := PerformInboundHandshakeWait(r.session, inConn, inboundHandshakeTimeout); err != nil {
			_ = inConn.Close()
			return fmt.Errorf("bridge inbound handshake: %w", err)
		}

		outConn, err := r.DialBridge("pair2")
		if err != nil {
			return fmt.Errorf("bridge dial (outbound): %w", err)
		}
		if err := PerformOutboundHandshakeOnConn(r.session, outConn); err != nil {
			_ = outConn.Close()
			return fmt.Errorf("bridge outbound handshake: %w", err)
		}

		logger.Info("Both directions reconnected via bridge (initiator)")
		return nil
	}

	outConn, err := r.DialBridge("pair1")
	if err != nil {
		return fmt.Errorf("bridge dial (outbound): %w", err)
	}
	if err := PerformOutboundHandshakeOnConn(r.session, outConn); err != nil {
		_ = outConn.Close()
		return fmt.Errorf("bridge outbound handshake: %w", err)
	}

	inConn, err := r.DialBridge("pair2")
	if err != nil {
		return fmt.Errorf("bridge dial (inbound): %w", err)
	}
	if err := PerformInboundHandshakeWait(r.session, inConn, inboundHandshakeTimeout); err != nil {
		_ = inConn.Close()
		return fmt.Errorf("bridge inbound handshake: %w", err)
	}

	logger.Info("Both directions reconnected via bridge (responder)")
	return nil
}

// reconnectDirectInitiator dials the peer, then accepts the return leg.
func (r *ReconnectManager) reconnectDirectInitiator(logger *slog.Logger) error {
	if err := r.dialPeerDirect(logger); err != nil {
		return err
	}

	if r.AcceptConn == nil {
		return fmt.Errorf("no accept function for inbound")
	}
	inConn, err := r.AcceptConn(30 * time.Second)
	if err != nil {
		return fmt.Errorf("accept inbound: %w", err)
	}
	if err := PerformInboundHandshake(r.session, inConn); err != nil {
		inConn.Close()
		return fmt.Errorf("inbound handshake: %w", err)
	}
	logger.Info("Both directions reconnected (initiator)")
	return nil
}

func (r *ReconnectManager) reconnectAsResponder() error {
	logger := r.logger.With("role", "responder")

	if r.useBridgeFirst() {
		return r.finishTransport(transportBridge, r.reconnectBridge(logger, false))
	}
	if r.mixedFirst() {
		return r.finishTransport(transportMixed, r.noteDirectFailure(logger, r.reconnectMixed(logger, false)))
	}
	return r.finishTransport(transportDirect, r.noteDirectFailure(logger, r.reconnectDirectResponder(logger)))
}

// dialPeerDirect runs the outbound handshake against the cached peer address, and
// once more against a fresh relay lookup when that fails.
func (r *ReconnectManager) dialPeerDirect(logger *slog.Logger) error {
	addr := ""
	if r.CachedPeerIP != "" && r.CachedPeerPort > 0 {
		addr = net.JoinHostPort(r.CachedPeerIP, fmt.Sprintf("%d", r.CachedPeerPort))
	}

	err := PerformOutboundHandshake(r.session, addr)
	if err == nil {
		return nil
	}
	logger.Debug("Cached address failed, trying relay lookup", "error", err)
	if r.RelayLookup == nil {
		return fmt.Errorf("outbound failed and no relay lookup: %w", err)
	}
	ip, port, lookupErr := r.RelayLookup(r.session.ExpectedPeerFingerprint)
	if lookupErr != nil {
		return fmt.Errorf("relay lookup failed: %w", lookupErr)
	}
	r.CachedPeerIP = ip
	r.CachedPeerPort = port
	addr = net.JoinHostPort(ip, fmt.Sprintf("%d", port))

	if err := PerformOutboundHandshake(r.session, addr); err != nil {
		return fmt.Errorf("outbound handshake failed: %w", err)
	}
	return nil
}

// reconnectDirectResponder accepts inbound, then dials outbound.
func (r *ReconnectManager) reconnectDirectResponder(logger *slog.Logger) error {
	if r.AcceptConn == nil {
		return fmt.Errorf("no accept function configured")
	}

	conn, err := r.AcceptConn(30 * time.Second)
	if err != nil {
		return fmt.Errorf("accept failed: %w", err)
	}
	if err := PerformInboundHandshake(r.session, conn); err != nil {
		conn.Close()
		return fmt.Errorf("inbound handshake: %w", err)
	}

	addr := ""
	if r.CachedPeerIP != "" && r.CachedPeerPort > 0 {
		addr = net.JoinHostPort(r.CachedPeerIP, fmt.Sprintf("%d", r.CachedPeerPort))
	} else if tcpAddr, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peerIP := tcpAddr.IP.String()
		if tcpAddr.Zone != "" {
			peerIP += "%" + tcpAddr.Zone
		}
		addr = net.JoinHostPort(peerIP, fmt.Sprintf("%d", r.CachedPeerPort))
	}
	if addr == "" {
		return fmt.Errorf("no peer address for outbound")
	}

	if err := PerformOutboundHandshake(r.session, addr); err != nil {
		return fmt.Errorf("outbound handshake: %w", err)
	}
	logger.Info("Both directions reconnected (responder)")
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
