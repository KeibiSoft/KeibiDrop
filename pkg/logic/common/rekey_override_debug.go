//go:build debug

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Debug-only KD_REKEY_BYTES/KD_REKEY_MSGS env overrides for rekey testing.

package common

import (
	"os"
	"strconv"
)

// envRekeyUint reads an unsigned-integer env var for the debug rekey override.
// It returns 0 when the value is unset or invalid, which keeps the production default.
func envRekeyUint(name string) uint64 {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// rekeyThresholdOverrideFromEnv reads KD_REKEY_BYTES/KD_REKEY_MSGS for the debug rekey
// threshold override. A zero value means "no override" for that threshold.
func rekeyThresholdOverrideFromEnv() (bytes uint64, msgs uint64) {
	return envRekeyUint("KD_REKEY_BYTES"), envRekeyUint("KD_REKEY_MSGS")
}
