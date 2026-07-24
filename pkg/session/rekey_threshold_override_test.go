// ABOUTME: Tests for the KD_REKEY_BYTES/KD_REKEY_MSGS debug threshold override clamp.
// ABOUTME: The override may only lower rotation thresholds, never disable them.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import "testing"

func TestApplyRekeyThresholdOverride(t *testing.T) {
	origB, origM := RekeyBytesThreshold, RekeyMsgsThreshold
	t.Cleanup(func() { RekeyBytesThreshold, RekeyMsgsThreshold = origB, origM })

	cases := []struct {
		name         string
		inB, inM     uint64
		wantB, wantM uint64
	}{
		{"zero leaves defaults", 0, 0, defaultRekeyBytes, defaultRekeyMsgs},
		{"below floor clamps up", 1, 1, minRekeyBytes, minRekeyMsgs},
		{"above default clamps down", 1 << 40, 1 << 40, defaultRekeyBytes, defaultRekeyMsgs},
		{"valid mid value applied", 4 << 20, 8 << 10, 4 << 20, 8 << 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			RekeyBytesThreshold, RekeyMsgsThreshold = defaultRekeyBytes, defaultRekeyMsgs
			gotB, gotM := ApplyRekeyThresholdOverride(tc.inB, tc.inM)
			if gotB != tc.wantB || RekeyBytesThreshold != tc.wantB {
				t.Errorf("bytes: got %d (var %d), want %d", gotB, RekeyBytesThreshold, tc.wantB)
			}
			if gotM != tc.wantM || RekeyMsgsThreshold != tc.wantM {
				t.Errorf("msgs: got %d (var %d), want %d", gotM, RekeyMsgsThreshold, tc.wantM)
			}
		})
	}
}

// Locks the shipped forward-secrecy posture: a release build rotates on the production thresholds,
// the debug override can never disable rotation, and in-band key-update (the eager entropy fold) is
// ON by default. A regression in any of these silently weakens forward secrecy.
func TestProductionDefaults(t *testing.T) {
	if defaultRekeyBytes != 1<<30 {
		t.Errorf("default rekey bytes = %d, want 1 GiB (1<<30)", defaultRekeyBytes)
	}
	if defaultRekeyMsgs != 1<<20 {
		t.Errorf("default rekey msgs = %d, want ~1M (1<<20)", defaultRekeyMsgs)
	}
	// Non-zero floors are what stop KD_REKEY_* from disabling rotation entirely.
	if minRekeyBytes == 0 || minRekeyMsgs == 0 {
		t.Fatal("rekey floors must be non-zero so the debug override can never disable rotation")
	}
	// Key-update (and the eager entropy fold) must ship enabled; the off-switch is a test-only seam.
	if !ownSupportsKeyUpdate() {
		t.Fatal("key-update must be ON by default: the shipped build must offer forward secrecy")
	}
}
