// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newFreeLaneKD(t *testing.T) (*KeibiDrop, *[]string) {
	t.Helper()
	kd := &KeibiDrop{logger: quietLogger(), wallet: &TokenWallet{}}
	kd.ConnectionMode = "bridge"
	events := &[]string{}
	kd.OnEvent = func(e string) { *events = append(*events, e) }
	return kd, events
}

func TestNoteSlowFetch_FreeLaneSaysItOncePerSession(t *testing.T) {
	kd, events := newFreeLaneKD(t)
	kd.noteSlowFetch(3 * time.Second)
	kd.noteSlowFetch(5 * time.Second)
	require.Equal(t, []string{"relay_slow:" + RelaySlowNotice}, *events, "one reminder, not one per stall")
	require.True(t, kd.relayThrottled.Load())
	require.Contains(t, RelaySlowNotice, TokensBuyURL)

	kd.resetRelaySignals()
	require.False(t, kd.relayThrottled.Load())
	kd.noteSlowFetch(3 * time.Second)
	require.Len(t, *events, 2, "the next session gets its own reminder")
}

func TestNoteSlowFetch_SilentOffTheBridge(t *testing.T) {
	for _, mode := range []string{"direct", "lan", ""} {
		kd, events := newFreeLaneKD(t)
		kd.ConnectionMode = mode
		kd.noteSlowFetch(10 * time.Second)
		require.Empty(t, *events, "mode %q", mode)
		require.False(t, kd.relayThrottled.Load(), "mode %q", mode)
	}
}

func TestNoteSlowFetch_SilentWithPriorityOrCredit(t *testing.T) {
	kd, events := newFreeLaneKD(t)
	ts := &tokenSession{}
	ts.paid.Store(true)
	kd.tokenSess = ts
	kd.noteSlowFetch(10 * time.Second)
	require.Empty(t, *events, "a paid lane is not the shared lane")
	require.False(t, kd.relayThrottled.Load())

	kd, events = newFreeLaneKD(t)
	kd.wallet = &TokenWallet{chains: []*walletChain{{Units: 10, Revealed: 2}}}
	kd.noteSlowFetch(10 * time.Second)
	require.Empty(t, *events, "credit in the wallet claims priority on the next reveal; no reminder")
}

func TestSessionState_ThrottledOnlyOnTheFreeBridgeLane(t *testing.T) {
	kd := &KeibiDrop{logger: quietLogger(), wallet: &TokenWallet{}}
	kd.relayThrottled.Store(true)
	// Not running: the state line is idle and never reports a throttle.
	require.False(t, kd.SessionState().Throttled)
}
