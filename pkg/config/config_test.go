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
)

func TestApplyEnvOverrides_PassphraseProtect(t *testing.T) {
	t.Setenv("KD_PASSPHRASE_PROTECT", "1")

	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	if !cfg.PassphraseProtect {
		t.Error("expected PassphraseProtect=true when KD_PASSPHRASE_PROTECT=1")
	}
}

func TestApplyEnvOverrides_PassphraseProtect_LongPrefix(t *testing.T) {
	t.Setenv("KEIBIDROP_PASSPHRASE_PROTECT", "1")

	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	if !cfg.PassphraseProtect {
		t.Error("expected PassphraseProtect=true when KEIBIDROP_PASSPHRASE_PROTECT=1")
	}
}

func TestApplyEnvOverrides_DefaultsNotSet(t *testing.T) {
	cfg := DefaultConfig()
	applyEnvOverrides(&cfg)

	if cfg.PassphraseProtect {
		t.Error("expected PassphraseProtect=false when env var not set")
	}
}

// TestSaveLoad_PrefetchOnOpenPersists verifies the on-demand/prefetch toggle
// survives a Save->Load round-trip. It is read on Load, but Save must also WRITE
// it — otherwise a UI toggle silently resets to on-demand next session.
func TestSaveLoad_PrefetchOnOpenPersists(t *testing.T) {
	t.Setenv("KEIBIDROP_CONFIG_DIR", t.TempDir())

	// Defaults: pure on-demand — prefetch off and auto-prefetch disabled (0).
	// Aggressive prefetch saturates constrained/relay links and freezes seeks,
	// so it is opt-in, not the default.
	if DefaultConfig().PrefetchOnOpen {
		t.Fatal("default PrefetchOnOpen should be false (on-demand)")
	}
	if DefaultConfig().PrefetchAutoMB != 0 {
		t.Fatalf("default PrefetchAutoMB should be 0 (on-demand), got %d", DefaultConfig().PrefetchAutoMB)
	}

	// Persist the FUSE toggles + a custom auto threshold.
	cfg := DefaultConfig()
	cfg.PrefetchOnOpen = true
	cfg.LiveCollab = true
	cfg.PrefetchAutoMB = 250
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// A fresh Load must observe all three (proves Save actually wrote them).
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.PrefetchOnOpen {
		t.Error("prefetch_on_open did not persist across Save/Load")
	}
	if !got.LiveCollab {
		t.Error("live_collab did not persist across Save/Load")
	}
	if got.PrefetchAutoMB != 250 {
		t.Errorf("prefetch_auto_mb did not persist: got %d want 250", got.PrefetchAutoMB)
	}
}
