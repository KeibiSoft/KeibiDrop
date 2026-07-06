// ABOUTME: Guards the proactive-rekey trigger in the health monitor: it fires OnRekeyNeeded only when idle and past the rekey threshold.
// ABOUTME: The actual re-handshake lives in the resilience layer; this just verifies the idle/threshold gating of the trigger.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

func TestHealthMonitorProactiveRekeyTrigger(t *testing.T) {
	old := RekeyBytesThreshold
	RekeyBytesThreshold = 1
	t.Cleanup(func() { RekeyBytesThreshold = old })

	key := randomKey(t)
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	// Move more than the idle delta so the session looks ACTIVE (serving) on the first check.
	_, err := sc.Write(randomBytes(t, rekeyIdleByteDelta+4096))
	require.NoError(t, err)
	require.True(t, sc.ShouldRekey())

	sess := &Session{Session: &SessionSockets{Outbound: sc}}
	// The monitor captures the socket pointers at creation; set the captured outbound
	// directly since this test builds the monitor without NewHealthMonitor.
	m := &HealthMonitor{session: sess, RekeyEnabled: true, outbound: sc}
	fired := 0
	m.OnRekeyNeeded = func() bool { fired++; return true }

	// A large byte delta since the mark means a transfer is moving (serving side): no rekey.
	require.False(t, m.maybeRekey(), "must not rekey while bytes are moving")
	require.Equal(t, 0, fired)

	// No new bytes since the mark: idle and over threshold, so it rotates.
	require.True(t, m.maybeRekey(), "should rekey once idle and over threshold")
	require.Equal(t, 1, fired)

	// An active pull suppresses the trigger entirely (tick returns before the rekey check).
	m.TransferStarted()
	m.tick()
	require.Equal(t, 1, fired, "no rekey during an active pull")
	m.TransferEnded()

	// Below threshold: no rekey even when idle.
	RekeyBytesThreshold = 1 << 62
	require.False(t, sc.ShouldRekey())
	require.False(t, m.maybeRekey(), "no rekey below threshold")
	require.Equal(t, 1, fired)
}
