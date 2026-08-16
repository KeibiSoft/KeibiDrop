// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
