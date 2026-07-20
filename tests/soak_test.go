// ABOUTME: Sustained-churn soak: many ratchet round trips over one connection must not leak
// ABOUTME: goroutines. Guards the always-on ratchet / rekey paths against a per-transfer leak.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

// TestSoak_RatchetChurnNoGoroutineLeak moves many payloads across the ratchet threshold on a single
// connection (no reconnect) and asserts the goroutine count is flat afterwards. Each round trip
// crosses the 1 MiB threshold, so the writer bumps its epoch every transfer: if any per-transfer or
// per-ratchet goroutine (a worker, a timer, a stray context) were left running, the count would
// climb with the round count. Heap is only logged, not asserted: every round trip shares a new file,
// so SyncTracker metadata grows legitimately. Reconnect-driven goroutine churn is covered separately
// by pkg/session TestHealthMonitor_NoGoroutineLeakAcrossRestarts.
func TestSoak_RatchetChurnNoGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping ratchet-churn soak in short mode")
	}

	// Ratchet ON (default). Drop the byte threshold so every ~1.5 MiB transfer forces an in-band
	// epoch bump; restore it so no later test inherits the lowered threshold.
	origBytes, origMsgs := session.RekeyBytesThreshold, session.RekeyMsgsThreshold
	session.RekeyBytesThreshold = 1 << 20
	t.Cleanup(func() { session.RekeyBytesThreshold, session.RekeyMsgsThreshold = origBytes, origMsgs })

	tp := SetupPeerPair(t, false)
	require := require.New(t)
	requireResilienceReady(t, tp)
	initiator, _ := rekeyRoles(t, tp)

	payload := bytes.Repeat([]byte("soak-ratchet-pl!"), 96*1024) // ~1.5 MiB, over the 1 MiB threshold

	// Warm up so every lazily-created goroutine/buffer exists before we snapshot the baseline.
	for i := 0; i < 3; i++ {
		rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, fmt.Sprintf("warm-a2b-%d.bin", i), payload)
		rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir, fmt.Sprintf("warm-b2a-%d.bin", i), payload)
	}
	runtime.GC()
	baseGo := runtime.NumGoroutine()
	var msBase runtime.MemStats
	runtime.ReadMemStats(&msBase)
	epochBefore := initiator.HealthMonitor.MaxWriterEpoch()

	const rounds = 40
	for i := 0; i < rounds; i++ {
		rekeyRoundTrip(t, tp.Alice, tp.AliceSaveDir, tp.Bob, tp.BobSaveDir, fmt.Sprintf("soak-a2b-%d.bin", i), payload)
		rekeyRoundTrip(t, tp.Bob, tp.BobSaveDir, tp.Alice, tp.AliceSaveDir, fmt.Sprintf("soak-b2a-%d.bin", i), payload)
	}

	epochAfter := initiator.HealthMonitor.MaxWriterEpoch()
	require.Greaterf(epochAfter, epochBefore+10, "soak must drive sustained ratchet churn: %d->%d", epochBefore, epochAfter)

	runtime.GC()
	time.Sleep(100 * time.Millisecond) // let any transient transfer goroutines finish
	runtime.GC()
	afterGo := runtime.NumGoroutine()
	var msAfter runtime.MemStats
	runtime.ReadMemStats(&msAfter)

	t.Logf("soak: %d rounds x2 x~1.5MiB | epoch %d->%d | goroutines %d->%d | heapInuse %d->%d KiB",
		rounds, epochBefore, epochAfter, baseGo, afterGo, msBase.HeapInuse/1024, msAfter.HeapInuse/1024)

	require.LessOrEqualf(afterGo, baseGo+3,
		"goroutine leak under sustained ratchet churn: base=%d after=%d (%d round trips)", baseGo, afterGo, rounds*2)
}
