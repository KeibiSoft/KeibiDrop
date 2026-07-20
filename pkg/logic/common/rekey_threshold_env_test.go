// ABOUTME: Tests the KD_REKEY_BYTES/KD_REKEY_MSGS env parser for the debug threshold knob.
// ABOUTME: Unset, empty, or unparseable values must yield 0 (leave the default in place).

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import "testing"

func TestEnvRekeyUint(t *testing.T) {
	cases := []struct {
		name string
		val  string
		want uint64
	}{
		{"empty", "", 0},
		{"valid", "1048576", 1048576},
		{"garbage", "notanumber", 0},
		{"negative", "-5", 0},
		{"floor value", "1", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KD_TEST_REKEY_UINT", tc.val)
			if got := envRekeyUint("KD_TEST_REKEY_UINT"); got != tc.want {
				t.Errorf("envRekeyUint(%q) = %d, want %d", tc.val, got, tc.want)
			}
		})
	}
}
