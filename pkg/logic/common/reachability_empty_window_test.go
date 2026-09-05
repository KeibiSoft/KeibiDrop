// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: An empty direct accept window must not override a fresh reachable verdict
// ABOUTME: from the relay probe, or an always-on creator pins itself to the bridge.

package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func probeVerdict(kd *KeibiDrop, reachable bool, age time.Duration) {
	kd.probedOn.Store(kd.LocalIPv6IP)
	kd.probedAt.Store(time.Now().Add(-age).UnixNano())
	kd.probedReachable.Store(reachable)
}

// Measured on the Timisoara container: its probe said reachable, the first
// auto-connect window closed empty because the laptop was not dialing yet, the
// mark went up, every later round skipped the direct window, and the laptop was
// told not to dial. Nothing could clear it but the 15 minute re-probe.
func TestEmptyAcceptWindow_KeepsAFreshReachableVerdict(t *testing.T) {
	kd := newRendezvousKD(t)
	probeVerdict(kd, true, time.Minute)
	kd.noteEmptyAcceptWindow()
	require.False(t, kd.InboundBlocked(), "an empty window is not evidence against a reachable probe")
}

func TestEmptyAcceptWindow_MarksBlockedWithoutAVerdict(t *testing.T) {
	kd := newRendezvousKD(t)
	kd.noteEmptyAcceptWindow()
	require.True(t, kd.InboundBlocked(), "with no probe the empty window is the only evidence")
}

func TestEmptyAcceptWindow_MarksBlockedOnAStaleOrNegativeVerdict(t *testing.T) {
	kd := newRendezvousKD(t)
	probeVerdict(kd, true, probeCacheTTL+time.Second)
	kd.noteEmptyAcceptWindow()
	require.True(t, kd.InboundBlocked(), "a verdict past its TTL does not protect the listener")

	kd = newRendezvousKD(t)
	probeVerdict(kd, false, time.Minute)
	kd.noteEmptyAcceptWindow()
	require.True(t, kd.InboundBlocked(), "a negative verdict agrees with the empty window")
}

func TestEmptyAcceptWindow_VerdictBindsToTheAddress(t *testing.T) {
	kd := newRendezvousKD(t)
	probeVerdict(kd, true, time.Minute)
	kd.probedOn.Store("2001:db8::1") // measured on another network
	kd.noteEmptyAcceptWindow()
	require.True(t, kd.InboundBlocked(), "a verdict from another address does not carry over")
}
