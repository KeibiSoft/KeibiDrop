// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

// ConnectionHealth represents the health state of the P2P connection.
type ConnectionHealth int32

const (
	HealthUnknown      ConnectionHealth = iota
	HealthHealthy                       // All heartbeats succeeding
	HealthDegraded                      // High latency or some failures
	HealthDisconnected                  // Connection lost, needs reconnection
)

// HealthMonitor monitors the health of a P2P connection via periodic heartbeats.
// When enough consecutive heartbeats fail, it triggers the OnDisconnect callback.
type HealthMonitor struct {
	session    *Session
	grpcClient bindings.KeibiServiceClient
	// inbound/outbound are captured at creation and immutable for this monitor's lifetime (a
	// fresh monitor is built after a reconnect swaps the sockets), so reading their byte stats
	// needs no lock even while a reconnect rewrites session.Session.
	inbound  *SecureConn
	outbound *SecureConn
	logger   *slog.Logger

	// State
	health           atomic.Int32 // ConnectionHealth
	lastRTT          atomic.Int64 // nanoseconds
	avgRTT           atomic.Int64 // exponential moving average
	consecutiveFails atomic.Int32
	seq              atomic.Uint64
	activeTransfers  atomic.Int32
	rekeyBytesMark   uint64 // session byte total at the previous tick; detects serving activity for the rekey idle gate

	// Configuration
	Interval    time.Duration // heartbeat interval (default 5s)
	Timeout     time.Duration // per-heartbeat timeout (default 3s)
	DegradedRTT time.Duration // RTT above this = degraded (default 500ms)
	MaxFailures int           // failures before disconnect (default 3)

	// Callbacks
	OnHealthChange func(old, new ConnectionHealth)
	OnDisconnect   func()
	// OnRekeyNeeded fires on an idle heartbeat tick when the session has passed its rekey
	// threshold, so the resilience layer can rotate keys via a re-handshake. Returns true only
	// when it actually initiated a rotation; on false the tick falls through to a normal
	// heartbeat, so liveness detection is never starved. Fires only when RekeyEnabled is set,
	// which the resilience layer ties to the absence of the in-band ratchet.
	OnRekeyNeeded func() bool
	RekeyEnabled  bool
	// ExtraEpoch, when set, reports the highest writer epoch of ratcheting conns OUTSIDE the
	// captured TCP pair (the QUIC control lane). One re-handshake re-derives ALL keys (TCP and
	// QUIC SEKs) and resets every epoch to 0, so a single rescue covers all lanes, but only if
	// the trigger observes them. Without this a hot QUIC lane would hold at the wrap guard with
	// no rescue. Called only on the monitor goroutine.
	ExtraEpoch func() uint16

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// NewHealthMonitor creates a new health monitor with default settings.
func NewHealthMonitor(session *Session, client bindings.KeibiServiceClient, logger *slog.Logger) *HealthMonitor {
	m := &HealthMonitor{
		session:     session,
		grpcClient:  client,
		logger:      logger.With("component", "health-monitor"),
		Interval:    5 * time.Second,
		Timeout:     5 * time.Second,
		DegradedRTT: 500 * time.Millisecond,
		MaxFailures: 5,
		// The monitor may always propose a rotation; onRekeyNeeded decides whether to act.
		// Defaulting on here (not at each call site) keeps the monitor onReconnected rebuilds
		// from silently disabling the near-wrap re-handshake for a key-update pair.
		RekeyEnabled: true,
	}
	if session != nil && session.Session != nil {
		m.inbound = session.Session.Inbound
		m.outbound = session.Session.Outbound
	}
	return m
}

// Start begins the heartbeat monitoring loop in a goroutine.
func (m *HealthMonitor) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.done = make(chan struct{})
	m.health.Store(int32(HealthHealthy))
	go m.runLoop()
	m.logger.Info("Health monitor started", "interval", m.Interval)
}

// Stop halts the health monitoring loop and waits for it to exit.
func (m *HealthMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	if m.done != nil {
		select {
		case <-m.done:
		case <-time.After(3 * time.Second):
			m.logger.Warn("Health monitor stop timed out")
		}
	}
	m.logger.Debug("Health monitor stopped")
}

func (m *HealthMonitor) TransferStarted() { m.activeTransfers.Add(1) }
func (m *HealthMonitor) TransferEnded() {
	if m.activeTransfers.Add(-1) < 0 {
		m.activeTransfers.Store(0)
	}
}

// Health returns the current connection health state.
func (m *HealthMonitor) Health() ConnectionHealth {
	return ConnectionHealth(m.health.Load())
}

// LastRTT returns the most recent round-trip time in nanoseconds.
func (m *HealthMonitor) LastRTT() time.Duration {
	return time.Duration(m.lastRTT.Load())
}

// AvgRTT returns the exponential moving average of RTT.
func (m *HealthMonitor) AvgRTT() time.Duration {
	return time.Duration(m.avgRTT.Load())
}

// MaxWriterEpoch returns the higher key epoch across both captured conns, or 0 if none. It
// advances when the in-band ratchet rotates either send direction, so a caller can confirm a
// rotation without a reconnect. The captured conns are immutable for this monitor's lifetime.
func (m *HealthMonitor) MaxWriterEpoch() uint16 {
	var e uint16
	if m.inbound != nil {
		e = m.inbound.WriterEpoch()
	}
	if m.outbound != nil {
		if oe := m.outbound.WriterEpoch(); oe > e {
			e = oe
		}
	}
	return e
}

func (m *HealthMonitor) runLoop() {
	defer close(m.done)
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.tick()
		}
	}
}

// rekeyIdleByteDelta is the most a truly-idle session's byte counters may grow between two
// ticks (heartbeat and keepalive scale). More than this means a transfer is in flight in EITHER
// direction (including the serving side, which activeTransfers does not track), so a rekey must
// not drop the connection.
const rekeyIdleByteDelta = 32 * 1024

// sessionBytes sums the bytes moved across both of the session's sockets. It is the idle
// signal for the proactive rekey: an in-progress serving transfer grows these counters
// even though activeTransfers (pull-side only) stays zero.
func (m *HealthMonitor) sessionBytes() uint64 {
	var total uint64
	if m.inbound != nil {
		bs, br, _, _ := m.inbound.GetStats()
		total += bs + br
	}
	if m.outbound != nil {
		bs, br, _, _ := m.outbound.GetStats()
		total += bs + br
	}
	return total
}

// shouldRekey reports whether either captured socket has passed the rekey threshold. It
// reads the captured pointers, not session.Session, so it does not race a reconnect.
func (m *HealthMonitor) shouldRekey() bool {
	return (m.inbound != nil && m.inbound.ShouldRekey()) ||
		(m.outbound != nil && m.outbound.ShouldRekey())
}

// maybeRekey advances the idle byte mark and, when the session is genuinely idle (no
// serving traffic since the last tick) and past the rekey threshold, asks the resilience
// layer to rotate keys. It returns true only when a rotation was actually initiated.
func (m *HealthMonitor) maybeRekey() bool {
	cur := m.sessionBytes()
	idle := cur >= m.rekeyBytesMark && cur-m.rekeyBytesMark <= rekeyIdleByteDelta
	m.rekeyBytesMark = cur
	// Near-wrap uses the captured conns' epoch (MaxWriterEpoch), race-free on the monitor
	// goroutine: only a key-update conn ratchets past epoch 0, so a high epoch already implies
	// the ratchet is on. Reading the live m.session here would race a reconnect socket swap.
	// ExtraEpoch folds in the QUIC lane, which ratchets independently of the captured pair.
	maxEpoch := m.MaxWriterEpoch()
	if m.ExtraEpoch != nil {
		if q := m.ExtraEpoch(); q > maxEpoch {
			maxEpoch = q
		}
	}
	// The volume trigger belongs to the legacy (no-ratchet) path only. On a key-update session
	// the byte counters are cumulative, so ShouldRekey latches true after one threshold;
	// rotation is already handled in band by the ratchet, and firing OnRekeyNeeded here would
	// drop the sockets of a healthy session. The near-wrap disjunct stays unconditional: the
	// deliberate ratchet-era rescue that resets the epoch space via an idle re-handshake.
	ratchetOn := (m.inbound != nil && m.inbound.UsesKeyUpdate()) ||
		(m.outbound != nil && m.outbound.UsesKeyUpdate())
	volumeDue := m.shouldRekey() && !ratchetOn
	if idle && m.OnRekeyNeeded != nil && (volumeDue || maxEpoch >= epochRehandshakeThreshold) {
		return m.OnRekeyNeeded()
	}
	return false
}

// tick runs one monitor cycle: skip while a pull is active; otherwise, when the session
// is genuinely idle and past the rekey threshold, trigger a proactive rekey instead of a
// heartbeat this tick; otherwise send a heartbeat.
func (m *HealthMonitor) tick() {
	if m.activeTransfers.Load() > 0 {
		m.consecutiveFails.Store(0)
		if m.RekeyEnabled {
			m.rekeyBytesMark = m.sessionBytes()
		}
		return
	}
	// A rotation skips the heartbeat this tick; anything else (disabled, not the initiator, on
	// cooldown, already reconnecting, or not yet idle) falls through so liveness detection is
	// never starved. Byte-work runs only when enabled, so a default peer never touches
	// session.Session on the tick path.
	if m.RekeyEnabled && m.maybeRekey() {
		return
	}
	if err := m.sendHeartbeat(); err != nil {
		m.handleFailure(err)
	}
}

func (m *HealthMonitor) sendHeartbeat() error {
	ctx, cancel := context.WithTimeout(m.ctx, m.Timeout)
	defer cancel()

	seqNum := m.seq.Add(1)
	start := time.Now()

	req := &bindings.HeartbeatRequest{
		Timestamp: uint64(start.UnixNano()), //nolint:gosec // G115: UnixNano is positive after 1970
		Seq:       seqNum,
	}

	resp, err := m.grpcClient.Heartbeat(ctx, req)
	if err != nil {
		return err
	}

	rtt := time.Since(start)
	m.lastRTT.Store(int64(rtt))
	m.updateAvgRTT(rtt)
	m.consecutiveFails.Store(0)

	// Validate response sequence
	if resp.Seq != seqNum {
		m.logger.Warn("Heartbeat sequence mismatch", "expected", seqNum, "got", resp.Seq)
	}

	// Update health based on RTT
	oldHealth := ConnectionHealth(m.health.Load())
	var newHealth ConnectionHealth
	if rtt > m.DegradedRTT {
		newHealth = HealthDegraded
	} else {
		newHealth = HealthHealthy
	}

	if oldHealth != newHealth {
		m.health.Store(int32(newHealth))
		if m.OnHealthChange != nil {
			m.OnHealthChange(oldHealth, newHealth)
		}
		m.logger.Debug("Health changed", "from", oldHealth, "to", newHealth, "rtt", rtt)
	}

	// Log clock skew if significant (>5 seconds; mobile clocks drift more)
	if resp.Timestamp > 0 {
		peerTime := time.Unix(0, int64(resp.Timestamp)) //nolint:gosec // G115: timestamp fits int64 until year 2262
		skew := time.Since(peerTime) - rtt/2
		if skew.Abs() > 5*time.Second {
			m.logger.Warn("Clock skew detected", "skew", skew)
		}
	}

	return nil
}

func (m *HealthMonitor) handleFailure(err error) {
	fails := m.consecutiveFails.Add(1)
	m.logger.Warn("Heartbeat failed", "consecutive", fails, "max", m.MaxFailures, "error", err)

	if int(fails) >= m.MaxFailures {
		oldHealth := ConnectionHealth(m.health.Load())
		m.health.Store(int32(HealthDisconnected))

		if oldHealth != HealthDisconnected {
			m.logger.Error("Connection lost", "failures", fails)
			if m.OnHealthChange != nil {
				m.OnHealthChange(oldHealth, HealthDisconnected)
			}
			if m.OnDisconnect != nil {
				m.OnDisconnect()
			}
		}
	} else {
		// Mark as degraded after first failure
		oldHealth := ConnectionHealth(m.health.Load())
		if oldHealth == HealthHealthy {
			m.health.Store(int32(HealthDegraded))
			if m.OnHealthChange != nil {
				m.OnHealthChange(oldHealth, HealthDegraded)
			}
		}
	}
}

func (m *HealthMonitor) updateAvgRTT(rtt time.Duration) {
	const alpha = 0.2 // smoothing factor
	oldAvg := m.avgRTT.Load()
	if oldAvg == 0 {
		m.avgRTT.Store(int64(rtt))
	} else {
		newAvg := int64(alpha*float64(rtt) + (1-alpha)*float64(oldAvg))
		m.avgRTT.Store(newAvg)
	}
}

// String returns a human-readable representation of the health state.
func (h ConnectionHealth) String() string {
	switch h {
	case HealthHealthy:
		return "healthy"
	case HealthDegraded:
		return "degraded"
	case HealthDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}
