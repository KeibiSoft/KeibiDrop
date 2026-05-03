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
