// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// RelayKeepalive maintains relay registration by periodically refreshing
// before the TTL expires. This ensures peers can always find each other.
type RelayKeepalive struct {
	kd     *KeibiDrop
	logger *slog.Logger

	// Configuration
	Interval time.Duration // Refresh interval (default 8 min, relay TTL is 10 min)

	// State
	lastRefresh  atomic.Int64 // Unix timestamp of last successful refresh
	failureCount atomic.Int32
	paused       atomic.Bool

	// Control
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
}

// NewRelayKeepalive creates a new relay keepalive manager.
func NewRelayKeepalive(kd *KeibiDrop, logger *slog.Logger) *RelayKeepalive {
	return &RelayKeepalive{
		kd:       kd,
		logger:   logger.With("component", "relay-keepalive"),
		Interval: 8 * time.Minute, // Refresh before 10 min TTL
	}
}

// Start begins the background refresh loop.
func (rk *RelayKeepalive) Start() {
	rk.mu.Lock()
	defer rk.mu.Unlock()

	if rk.cancel != nil {
		return // Already running
	}

	rk.ctx, rk.cancel = context.WithCancel(context.Background())
	go rk.loop()
	rk.logger.Info("Relay keepalive started", "interval", rk.Interval)
}

// Stop halts the background refresh loop.
func (rk *RelayKeepalive) Stop() {
	rk.mu.Lock()
	defer rk.mu.Unlock()

	if rk.cancel != nil {
		rk.cancel()
		rk.cancel = nil
		rk.logger.Debug("Relay keepalive stopped")
	}
}

// Pause temporarily disables refresh (e.g., when disconnected).
func (rk *RelayKeepalive) Pause() {
	rk.paused.Store(true)
	rk.logger.Debug("Relay keepalive paused")
}

// Resume re-enables refresh.
func (rk *RelayKeepalive) Resume() {
	rk.paused.Store(false)
	rk.logger.Debug("Relay keepalive resumed")
}

// ForceRefresh immediately refreshes the relay registration.
// Call this on IP change or reconnection.
func (rk *RelayKeepalive) ForceRefresh() error {
	rk.mu.Lock()
	defer rk.mu.Unlock()
	return rk.refresh()
}

// LastRefresh returns the timestamp of the last successful refresh.
func (rk *RelayKeepalive) LastRefresh() time.Time {
	ts := rk.lastRefresh.Load()
	if ts == 0 {
		return time.Time{}
	}
	return time.Unix(ts, 0)
}

// FailureCount returns the number of consecutive refresh failures.
func (rk *RelayKeepalive) FailureCount() int {
	return int(rk.failureCount.Load())
}

func (rk *RelayKeepalive) loop() {
	ticker := time.NewTicker(rk.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-rk.ctx.Done():
			return
		case <-ticker.C:
			if rk.paused.Load() {
				continue
			}
			rk.mu.Lock()
			_ = rk.refresh()
			rk.mu.Unlock()
		}
	}
}

func (rk *RelayKeepalive) refresh() error {
	if err := rk.kd.registerRoomToRelay(); err != nil {
		failures := rk.failureCount.Add(1)
		rk.logger.Warn("Relay keepalive refresh failed",
			"error", err,
			"consecutive_failures", failures)
		return err
	}

	rk.lastRefresh.Store(time.Now().Unix())
	rk.failureCount.Store(0)
	rk.logger.Debug("Relay registration refreshed")
	return nil
}

// CheckIPChange detects if the local IP has changed and refreshes if needed.
// Call this periodically or on network state change.
func (rk *RelayKeepalive) CheckIPChange() error {
	newIP, err := GetGlobalIPv6()
	if err != nil {
		return err
	}

	if newIP != rk.kd.LocalIPv6IP {
		rk.logger.Info("IP changed, refreshing relay registration",
			"old", rk.kd.LocalIPv6IP,
			"new", newIP)
		rk.kd.LocalIPv6IP = newIP
		return rk.ForceRefresh()
	}

	return nil
}
