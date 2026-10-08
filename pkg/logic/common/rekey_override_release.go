//go:build !debug

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: No-op stub: the rekey threshold env override is debug-only.

package common

// rekeyThresholdOverrideFromEnv always returns "no override" in a non-debug build.
// The KD_REKEY_BYTES/KD_REKEY_MSGS env read is debug-only.
func rekeyThresholdOverrideFromEnv() (bytes uint64, msgs uint64) {
	return 0, 0
}
