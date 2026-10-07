// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Guards the keepalive cadence against the relay's registration TTL.

package common

import (
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// The relay drops registrations it has not seen recently, on purpose: that is what stops a dead
// or idle client from hoarding a slot. So the cadence is bounded on both sides. Too slow and a
// live peer goes missing from /fetch between refreshes, which reads as "peer not found" on the
// other side; too fast and we lean on the relay's rate limits for no gain, since the drop is the
// feature. A failed refresh is not retried before the next tick, so one miss has to fit as well.
func TestRelayKeepalive_CadenceFitsTheRelayTTL(t *testing.T) {
	rk := NewRelayKeepalive(&KeibiDrop{}, testkit.DiscardLogger())

	require.Less(t, rk.Interval, relayEntryTTL,
		"a live peer must never be absent from the relay between refreshes")
	require.Less(t, 2*rk.Interval, relayEntryTTL,
		"one missed refresh must still re-register inside the TTL")
	require.GreaterOrEqual(t, rk.Interval, 1*time.Minute,
		"refreshing faster than this only spends the relay's rate limit; the drop is intended")
}
