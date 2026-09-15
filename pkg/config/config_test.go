// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Unit tests for the config package.
// ABOUTME: Verifies env-var overrides and flag binding for all Config fields.

package config

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

func TestApplyEnvOverrides_PassphraseProtect(t *testing.T) {
	t.Setenv("KD_PASSPHRASE_PROTECT", "1")

	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	require.True(t, cfg.PassphraseProtect, "KD_PASSPHRASE_PROTECT=1 must set PassphraseProtect")
}

func TestApplyEnvOverrides_PassphraseProtect_LongPrefix(t *testing.T) {
	t.Setenv("KEIBIDROP_PASSPHRASE_PROTECT", "1")

	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	require.True(t, cfg.PassphraseProtect, "KEIBIDROP_PASSPHRASE_PROTECT=1 must set PassphraseProtect")
}

func TestApplyEnvOverrides_DefaultsNotSet(t *testing.T) {
	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	require.False(t, cfg.PassphraseProtect, "PassphraseProtect must be false when env var not set")
}

// TestSaveLoad_PrefetchOnOpenPersists verifies on-demand/prefetch toggle
// survives Save+Load. Save must WRITE it, not just Load read it, else UI toggle
// silently resets to on-demand next session.
func TestSaveLoad_PrefetchOnOpenPersists(t *testing.T) {
	t.Setenv("KEIBIDROP_CONFIG_DIR", t.TempDir())

	// Prefetch is opt-in. It saturates constrained and relay links and
	// freezes seeks, so the default stays on-demand.
	require.False(t, DefaultConfig().PrefetchOnOpen, "default PrefetchOnOpen must be false")
	require.Zero(t, DefaultConfig().PrefetchAutoMB, "default PrefetchAutoMB must be 0")

	cfg := DefaultConfig()
	cfg.PrefetchOnOpen = true
	cfg.LiveCollab = true
	cfg.PrefetchAutoMB = 250
	require.NoError(t, Save(cfg))

	// A fresh Load proves Save wrote all three, not just read them.
	testkit.Run(t, func() error {
		got := testkit.Must(Load())
		return fp.All(
			fp.True("prefetch_on_open persisted", got.PrefetchOnOpen),
			fp.True("live_collab persisted", got.LiveCollab),
			fp.Equal("prefetch_auto_mb persisted", got.PrefetchAutoMB, 250),
		)
	})
}

// TestReadAheadWindowMB_DefaultAndOverride verifies the read-ahead window is on
// by default, so video does not stall over a high-RTT link. The env override is
// the WAN-benchmark A/B knob.
func TestReadAheadWindowMB_DefaultAndOverride(t *testing.T) {
	require.Equal(t, 64, DefaultConfig().ReadAheadWindowMB, "default ReadAheadWindowMB must be 64")

	// Env override to a larger window, for example a fat link.
	t.Run("override", func(t *testing.T) {
		t.Setenv("KEIBIDROP_READ_AHEAD_WINDOW_MB", "128")
		cfg := DefaultConfig()
		applyEnvOverrides(&cfg)
		require.Equal(t, 128, cfg.ReadAheadWindowMB, "ReadAheadWindowMB must be 128 from env")
	})

	// Env override to 0 disables read-ahead. This is the benchmark baseline.
	t.Run("disable", func(t *testing.T) {
		t.Setenv("KEIBIDROP_READ_AHEAD_WINDOW_MB", "0")
		cfg := DefaultConfig()
		applyEnvOverrides(&cfg)
		require.Equal(t, 0, cfg.ReadAheadWindowMB, "ReadAheadWindowMB must be 0 from env")
	})
}

// TestSaveLoad_ReadAheadWindowPersists verifies read-ahead window survives
// Save+Load. Save must WRITE it, not just Load read it.
func TestSaveLoad_ReadAheadWindowPersists(t *testing.T) {
	t.Setenv("KEIBIDROP_CONFIG_DIR", t.TempDir())

	cfg := DefaultConfig()
	cfg.ReadAheadWindowMB = 96
	require.NoError(t, Save(cfg))

	testkit.Run(t, func() error {
		got := testkit.Must(Load())
		return fp.Equal("read_ahead_window_mb persisted", got.ReadAheadWindowMB, 96)
	})
}

func TestDirectoriesToEnsure_SkipsMountPathOnWindows(t *testing.T) {
	cfg := Config{SavePath: "/save", MountPath: "/mnt", LogFile: "/logs/kd.log"}

	win := directoriesToEnsure(cfg, "windows")
	other := directoriesToEnsure(cfg, "linux")

	// Steps, not All. Both checks were t.Fatalf, and the second indexes a slice
	// the first does not prove non-empty.
	testkit.Run(t, func() error {
		return fp.Steps(
			func() error {
				return fp.Each("windows dirs", win, func(d string) error {
					return fp.NotEqual("must not pre-create mount_path", d, cfg.MountPath)
				})
			},
			func() error {
				return fp.Equal("non-windows creates mount_path", other[len(other)-1], cfg.MountPath)
			},
		)
	})
}

// One default pair meant the second surface to start could not bind. The app
// keeps the old pair.
func TestUseAgentPorts_MovesOnlyTheUnchosenDefaults(t *testing.T) {
	cfg := DefaultConfig()
	UseAgentPorts(&cfg)

	require.Equal(t, AgentInboundPort, cfg.InboundPort)
	require.Equal(t, AgentOutboundPort, cfg.OutboundPort)
	require.NotEqual(t, InboundPort, cfg.InboundPort, "agent and app still share the inbound port")
	require.NotEqual(t, OutboundPort, cfg.OutboundPort, "agent and app still share the outbound port")
	require.True(t, ValidPeerPort(cfg.InboundPort))
	require.True(t, ValidPeerPort(cfg.OutboundPort))
}

// No two surfaces may fall back to the same pair.
func TestSurfacePorts_NoTwoSurfacesShareADefault(t *testing.T) {
	pairs := map[string][2]int{
		"app":   {InboundPort, OutboundPort},
		"agent": {AgentInboundPort, AgentOutboundPort},
		"mcp":   {MCPInboundPort, MCPOutboundPort},
	}

	seen := map[int]string{}
	for surface, pair := range pairs {
		for _, port := range pair {
			if other, taken := seen[port]; taken {
				t.Fatalf("%s and %s both default to port %d", surface, other, port)
			}
			seen[port] = surface
			require.True(t, ValidPeerPort(port), "%s port %d is outside 26000-27000", surface, port)
		}
	}
}

// A NAS that forwards 26500 keeps something listening there.
func TestUseAgentPorts_LeavesAChosenPortAlone(t *testing.T) {
	cfg := DefaultConfig()
	cfg.InboundPort = 26500
	cfg.OutboundPort = 26501
	UseAgentPorts(&cfg)

	require.Equal(t, 26500, cfg.InboundPort)
	require.Equal(t, 26501, cfg.OutboundPort)
}

// The environment is the documented knob, so what it asks for has to survive.
func TestUseAgentPorts_EnvironmentWins(t *testing.T) {
	t.Setenv("KD_INBOUND_PORT", "26431")
	t.Setenv("KD_OUTBOUND_PORT", "26432")

	cfg := DefaultConfig()
	UseAgentPorts(&cfg)

	require.Equal(t, InboundPort, cfg.InboundPort)
	require.Equal(t, OutboundPort, cfg.OutboundPort)
}
