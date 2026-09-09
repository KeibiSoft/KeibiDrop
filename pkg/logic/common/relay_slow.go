// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import "time"

// RelaySlowNotice is said once per session on the free relay lane, the first
// time a reader waits seconds for one fetch. The state line carries the short
// form ("shared right now") for as long as the session lasts.
const RelaySlowNotice = "Reads are slow because the relay lane is shared right now. A data pack gives you the fast lane: " + TokensBuyURL

// paidLane reports a session that holds priority on the bridge, or has credit
// to claim it. Reminders about the shared lane never go to these.
func (kd *KeibiDrop) paidLane() bool {
	if ts := kd.currentTokenSession(); ts != nil && ts.paid.Load() {
		return true
	}
	return kd.Wallet().pickFunded() != nil
}

// noteSlowFetch is the filesystem's report that a reader waited on one demand
// fetch for longer than filesystem.SlowFetchNotice. Off the bridge, or with
// priority, a slow fetch is the link's business and nothing is said. On the
// free lane it is the shared lane doing what it does: mark the session
// throttled for the state line, and tell the person once per session.
func (kd *KeibiDrop) noteSlowFetch(waited time.Duration) {
	if kd.ConnectionMode != "bridge" || kd.paidLane() {
		return
	}
	kd.relayThrottled.Store(true)
	if !kd.relaySlowSignalled.CompareAndSwap(false, true) {
		return
	}
	kd.logger.Info("Slow fetch on the free relay lane", "waited", waited)
	kd.emitEvent("relay_slow:" + RelaySlowNotice)
}

// resetRelaySignals forgets the throttle marks at a session boundary, so the
// next session gets its own one reminder.
func (kd *KeibiDrop) resetRelaySignals() {
	kd.relayThrottled.Store(false)
	kd.relaySlowSignalled.Store(false)
}
