//go:build !debug

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: No-op stub: the rekey threshold env override is debug-only.

package common

// rekeyThresholdOverrideFromEnv always returns "no override" in a plain (non-debug)
// build; the KD_REKEY_BYTES/KD_REKEY_MSGS env read is debug-only.
func rekeyThresholdOverrideFromEnv() (bytes uint64, msgs uint64) {
	return 0, 0
}
