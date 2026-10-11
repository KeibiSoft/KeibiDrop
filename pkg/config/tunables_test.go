// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.
//
// ABOUTME: The Settings card's values: each key takes only its own kind and range,
// ABOUTME: a refused value changes nothing, and Reset puts every one back to its default.

package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetTunable_TakesEveryKeyTheSettingsCardSends(t *testing.T) {
	c := DefaultConfig()
	for key, value := range map[string]string{
		"share_read_only": "true", "mount_read_only": "true", "preserve_metadata": "true",
		"strict_mode": "true", "update_check": "false", "prefetch_on_open": "true",
		"live_collab": "true", "scan_shared_on_start": "true",
		"read_ahead_window_mb": "128", "prefetch_auto_mb": "256", "rescan_shared_seconds": " 60 ",
	} {
		require.NoError(t, c.SetTunable(key, value), key)
	}
	require.True(t, c.ShareReadOnly && c.MountReadOnly && c.PreserveMetadata && c.StrictMode)
	require.True(t, c.PrefetchOnOpen && c.LiveCollab && c.ScanSharedOnStart)
	require.False(t, c.UpdateCheck)
	require.Equal(t, 128, c.ReadAheadWindowMB)
	require.Equal(t, 256, c.PrefetchAutoMB)
	require.Equal(t, 60, c.RescanSharedSeconds)
}

func TestSetTunable_RefusesAndChangesNothing(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"strict_mode", "yes"},
		{"strict_mode", "1"},
		{"read_ahead_window_mb", "-1"},
		{"read_ahead_window_mb", "1025"},
		{"read_ahead_window_mb", "12.5"},
		{"prefetch_auto_mb", "lots"},
		{"rescan_shared_seconds", "3"},
		{"rescan_shared_seconds", "86401"},
		{"relay", "https://example.com"},
		{"inbound_port", "26001"},
	} {
		c := DefaultConfig()
		require.Error(t, c.SetTunable(tc.key, tc.value), "%s=%s", tc.key, tc.value)
		require.Equal(t, DefaultConfig(), c, "%s=%s changed the config", tc.key, tc.value)
	}
}

func TestSetTunable_ZeroTurnsTheNumbersOff(t *testing.T) {
	c := DefaultConfig()
	for _, key := range []string{"read_ahead_window_mb", "prefetch_auto_mb", "rescan_shared_seconds"} {
		require.NoError(t, c.SetTunable(key, "0"), key)
	}
	require.Zero(t, c.ReadAheadWindowMB+c.PrefetchAutoMB+c.RescanSharedSeconds)
}

func TestResetTunables_PutsTheDefaultsBackAndKeepsTheRest(t *testing.T) {
	c := DefaultConfig()
	c.Relay, c.SavePath, c.InboundPort, c.PassphraseProtect = "https://relay.example", "/data", 26101, true
	for key, value := range map[string]string{
		"share_read_only": "true", "strict_mode": "true", "update_check": "false", "live_collab": "true",
		"read_ahead_window_mb": "0", "prefetch_auto_mb": "64", "rescan_shared_seconds": "0",
	} {
		require.NoError(t, c.SetTunable(key, value), key)
	}
	c.ResetTunables()

	want := DefaultConfig()
	want.Relay, want.SavePath, want.InboundPort, want.PassphraseProtect = "https://relay.example", "/data", 26101, true
	require.Equal(t, want, c)
}
