//go:build debug

// ABOUTME: Tests the KD_REKEY_BYTES/KD_REKEY_MSGS env parser for the debug threshold knob.
// ABOUTME: Unset, empty, or unparseable values must yield 0 (leave the default in place).

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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
			assert.Equal(t, tc.want, envRekeyUint("KD_TEST_REKEY_UINT"))
		})
	}
}
