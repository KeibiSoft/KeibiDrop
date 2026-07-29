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

// The near-wrap disjunct of maybeRekey must fire the re-handshake below the byte threshold: a
// key-update conn ratcheted to the wrap guard has to rotate on a fresh epoch-0 key.
func TestHealthMonitorNearWrapTriggersRekey(t *testing.T) {
	// Byte threshold out of reach so shouldRekey() is false; only near-wrap can fire.
	old := RekeyBytesThreshold
	RekeyBytesThreshold = 1 << 62
	t.Cleanup(func() { RekeyBytesThreshold = old })

	key := randomKey(t)
	sc := NewSecureConn(&writeSink{}, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetWriterEpochForTest(epochRehandshakeThreshold)
	require.False(t, sc.ShouldRekey(), "below the byte threshold, so only near-wrap can fire")

	sess := &Session{Session: &SessionSockets{Outbound: sc}}
	m := &HealthMonitor{session: sess, RekeyEnabled: true, outbound: sc}
	fired := 0
	m.OnRekeyNeeded = func() bool { fired++; return true }

	require.True(t, m.maybeRekey(),
		"a key-update conn near the wrap guard must fire the re-handshake below the byte threshold")
	require.Equal(t, 1, fired)
}

// The QUIC lane ratchets independently of the TCP pair, so the near-wrap rescue must also fire when
// only the ExtraEpoch (QUIC) source nears the guard while the TCP pair sits at epoch 0.
func TestHealthMonitorQUICNearWrapTriggersRekey(t *testing.T) {
	old := RekeyBytesThreshold
	RekeyBytesThreshold = 1 << 62
	t.Cleanup(func() { RekeyBytesThreshold = old })

	key := randomKey(t)
	sc := NewSecureConn(&writeSink{}, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	require.Equal(t, uint16(0), sc.WriterEpoch(), "TCP pair stays at epoch 0 in this scenario")

	sess := &Session{Session: &SessionSockets{Outbound: sc}}
	m := &HealthMonitor{session: sess, RekeyEnabled: true, outbound: sc}
	fired := 0
	m.OnRekeyNeeded = func() bool { fired++; return true }

	// No extra source yet: nothing fires.
	require.False(t, m.maybeRekey(), "epoch 0 everywhere must not rekey")
	require.Equal(t, 0, fired)

	// QUIC lane near the wrap guard: the rescue must fire even though TCP is at epoch 0.
	m.ExtraEpoch = func() uint16 { return epochRehandshakeThreshold }
	require.True(t, m.maybeRekey(),
		"a QUIC-lane epoch at the re-handshake threshold must fire the rescue")
	require.Equal(t, 1, fired)
}

// A ratcheting (key-update) session must NOT fire the volume-based re-handshake: its byte counters
// are cumulative and never reset by a ratchet, so ShouldRekey latches true while rotation is already
// handled in band. Only the near-wrap disjunct may re-handshake it.
func TestHealthMonitorVolumeTriggerGatedOffWhenRatcheting(t *testing.T) {
	old := RekeyBytesThreshold
	RekeyBytesThreshold = 1
	t.Cleanup(func() { RekeyBytesThreshold = old })

	key := randomKey(t)
	sc := NewSecureConn(&writeSink{}, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetKeyUpdate(true)
	// make-before-break checks thresholds BEFORE sealing: the first (large) frame seals at epoch 0,
	// the second carries the rotation.
	_, err := sc.Write(randomBytes(t, rekeyIdleByteDelta+4096))
	require.NoError(t, err)
	_, err = sc.Write(randomBytes(t, 64))
	require.NoError(t, err)
	require.True(t, sc.ShouldRekey(), "cumulative counters must be past the threshold")
	require.GreaterOrEqual(t, sc.WriterEpoch(), uint16(1), "the second write must have carried a rotation")

	sess := &Session{Session: &SessionSockets{Outbound: sc}}
	m := &HealthMonitor{session: sess, RekeyEnabled: true, outbound: sc}
	fired := 0
	m.OnRekeyNeeded = func() bool { fired++; return true }

	require.False(t, m.maybeRekey(), "first check only sets the idle mark")
	require.False(t, m.maybeRekey(), "idle and over threshold, but ratcheting: no re-handshake")
	require.Equal(t, 0, fired)
}
