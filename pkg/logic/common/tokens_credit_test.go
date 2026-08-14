// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Tests TokensCreditStatus level mapping against the wallet
// ABOUTME: thresholds: ok, low, critical, empty, and the honest notices.

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTokensCreditStatus_Levels(t *testing.T) {
	kd := newTokenTestKD(t, "") // isolated wallet, never the real config dir

	st := kd.TokensCreditStatus()
	require.Equal(t, "empty", st.Level)
	require.Contains(t, st.Notice, "free tier")
	require.NotEmpty(t, st.BuyURL)

	c, err := kd.Wallet().Add(encodeTokenCode(testSeed(t), 6000)) // ~58 GB
	require.NoError(t, err)
	st = kd.TokensCreditStatus()
	require.Equal(t, "ok", st.Level)
	require.Empty(t, st.Notice)
	require.InDelta(t, 58.6, st.WalletGB, 1.0)

	kd.Wallet().markRevealed(c, 1500, false) // 4500 units, under the 50 GB mark
	st = kd.TokensCreditStatus()
	require.Equal(t, "low", st.Level)
	require.Contains(t, st.Notice, "Top up")

	kd.Wallet().markRevealed(c, 5900, false) // 100 units left, under the 2 GB mark
	st = kd.TokensCreditStatus()
	require.Equal(t, "critical", st.Level)
	require.Contains(t, st.Notice, "free tier")

	kd.Wallet().markRevealed(c, 6000, false) // dry
	st = kd.TokensCreditStatus()
	require.Equal(t, "empty", st.Level)
}
