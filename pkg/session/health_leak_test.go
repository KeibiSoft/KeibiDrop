// ABOUTME: Goroutine-leak guard for the health monitor across the Start/Stop churn a reconnect
// ABOUTME: drives: onReconnected stops the old monitor and starts a fresh one on every reconnect.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"runtime"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// A reconnect stops the old monitor and starts a fresh one; if Stop fails to reap the runLoop
// goroutine, a pair reconnecting on every blip leaks one per reconnect. Interval is out of reach so
// the nil-client heartbeat never fires, isolating lifecycle from the tick.
func TestHealthMonitor_NoGoroutineLeakAcrossRestarts(t *testing.T) {
	log := testkit.DiscardLogger()
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		m := NewHealthMonitor(&Session{}, nil, log)
		m.Interval = time.Hour // ticker must not fire: sendHeartbeat would nil-deref the client
		m.Start()
		m.Stop() // blocks until runLoop closes m.done, so the goroutine is reaped synchronously
	}

	runtime.GC()
	time.Sleep(50 * time.Millisecond) // let any transient runtime goroutines settle
	after := runtime.NumGoroutine()
	require.LessOrEqualf(t, after, base+3,
		"health monitor Start/Stop churn (one per reconnect) must not leak goroutines: base=%d after=%d",
		base, after)
}
