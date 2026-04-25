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
	logger     *slog.Logger

	// State
	health           atomic.Int32 // ConnectionHealth
	lastRTT          atomic.Int64 // nanoseconds
	avgRTT           atomic.Int64 // exponential moving average
	consecutiveFails atomic.Int32
	seq              atomic.Uint64

	// Configuration
	Interval    time.Duration // heartbeat interval (default 5s)
	Timeout     time.Duration // per-heartbeat timeout (default 3s)
	DegradedRTT time.Duration // RTT above this = degraded (default 500ms)
	MaxFailures int           // failures before disconnect (default 3)

	// Callbacks
	OnHealthChange func(old, new ConnectionHealth)
	OnDisconnect   func()

	// Control
	ctx    context.Context
	cancel context.CancelFunc
}

// NewHealthMonitor creates a new health monitor with default settings.
func NewHealthMonitor(session *Session, client bindings.KeibiServiceClient, logger *slog.Logger) *HealthMonitor {
	return &HealthMonitor{
		session:     session,
		grpcClient:  client,
		logger:      logger.With("component", "health-monitor"),
		Interval:    5 * time.Second,
		Timeout:     5 * time.Second,
		DegradedRTT: 500 * time.Millisecond,
		MaxFailures: 5,
	}
}

// Start begins the heartbeat monitoring loop in a goroutine.
func (m *HealthMonitor) Start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.health.Store(int32(HealthHealthy))
	go m.runLoop()
	m.logger.Info("Health monitor started", "interval", m.Interval)
}

// Stop halts the health monitoring loop.
func (m *HealthMonitor) Stop() {
	if m.cancel != nil {
		m.cancel()
		m.logger.Debug("Health monitor stopped")
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

func (m *HealthMonitor) runLoop() {
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if err := m.sendHeartbeat(); err != nil {
				m.handleFailure(err)
			}
		}
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

	// Log clock skew if significant (>5 seconds — mobile clocks drift more)
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
