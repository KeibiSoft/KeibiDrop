// ABOUTME: Guards that proactive re-handshake rekeying is OFF by default and only enabled by KD_PROACTIVE_REKEY.
// ABOUTME: Off-by-default keeps the shipped behavior unchanged until the fleet is upgraded and an operator opts in.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProactiveRekeyEnabledFlag(t *testing.T) {
	t.Setenv("KD_PROACTIVE_REKEY", "")
	require.False(t, proactiveRekeyEnabled(), "must be off by default")

	t.Setenv("KD_PROACTIVE_REKEY", "1")
	require.True(t, proactiveRekeyEnabled())

	t.Setenv("KD_PROACTIVE_REKEY", "true")
	require.True(t, proactiveRekeyEnabled())

	t.Setenv("KD_PROACTIVE_REKEY", "0")
	require.False(t, proactiveRekeyEnabled())
}
