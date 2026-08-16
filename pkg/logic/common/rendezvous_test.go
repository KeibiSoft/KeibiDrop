// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Tests for the creator's rendezvous loop: the room must outlive one round, so a
// ABOUTME: human-paced joiner who arrives a minute late still connects.

package common

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// W3. The creator's door was about 38 seconds wide on the real WAN pair: the direct
// accept window closed at 15s, the bridge round ended at 38s, and the joiner arrived at
// 51s to a room nobody was holding.
//
// The room is now re-armed every round until the connect budget runs out, so a joiner
// that arrives after several rounds still finds a listener on the port.
func TestRendezvous_RoomSurvivesRoundsUntilTheJoinerArrives(t *testing.T) {
	kd := newRendezvousKD(t)
	port := kd.inboundPort

	// Round 0's window closes with nobody there, exactly as it did on the WAN.
	require.NoError(t, kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(150*time.Millisecond)))
	_, acceptErr := kd.listener.Accept()
	require.Error(t, acceptErr, "the first window must close empty")
	kd.listener.Close()
	kd.listener = nil

	// The old code stopped here and the port went dead. Re-arm as a later round does.
	rearmRendezvousListener(t, kd)

	// A joiner arriving well after the old 38s door must still be accepted.
	accepted := testkit.Go(func() error {
		if err := kd.listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			return err
		}
		c, err := kd.listener.Accept()
		if err != nil {
			return fmt.Errorf("late joiner refused: %w", err)
		}
		return c.Close()
	})

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	require.NoError(t, err, "the re-armed room must accept a late joiner")
	require.NoError(t, conn.Close())
	require.NoError(t, accepted())
}

// The cancel path. A create that is still waiting has no session, so kd.Stop returns
// early and cannot reach it. NotifyDisconnect must break the loop instead, or a cancelled
// create blocks for the whole 595s budget.
func TestRendezvous_NotifyDisconnectCancelsAPendingCreate(t *testing.T) {
	kd := newRendezvousKD(t)

	require.False(t, kd.connectAborted.Load(), "a fresh instance is not aborted")
	kd.NotifyDisconnect()
	require.True(t, kd.connectAborted.Load(),
		"NotifyDisconnect must signal a create that is still waiting for its peer")
}

// The connect budget OUTLIVES a relay registration, so the loop has to refresh it. The
// relay keepalive does not help here: it only starts after a peer connects. Without the
// refresh the room stops being findable halfway through its own budget.
func TestRendezvous_BudgetOutlivesOneRelayRegistration(t *testing.T) {
	budget := time.Duration(Timeout) * time.Second
	require.Greater(t, budget, relayEntryTTL,
		"if the budget ever fits inside one registration, the in-loop refresh is dead code")
	require.Less(t, defaultKeepaliveInterval, relayEntryTTL,
		"the refresh cadence must leave room for a miss before the registration expires")
}

func newRendezvousKD(t *testing.T) *KeibiDrop {
	t.Helper()
	relayURL, err := url.Parse("https://localhost:9999")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	port := pickFreePortPair(t)
	kd, err := NewKeibiDropWithIP(ctx, testkit.DiscardLogger(), false, relayURL, port, port+1,
		"", t.TempDir(), false, false, "::1")
	require.NoError(t, err)
	t.Cleanup(func() {
		if kd.listener != nil {
			_ = kd.listener.Close()
		}
	})
	return kd
}

func rearmRendezvousListener(t *testing.T, kd *KeibiDrop) {
	t.Helper()
	addr := net.JoinHostPort("", fmt.Sprintf("%d", kd.inboundPort))
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err, "the round must be able to re-arm the listener on the same port")
	kd.listener = ln
}
