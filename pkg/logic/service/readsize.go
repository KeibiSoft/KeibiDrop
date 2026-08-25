// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Bounds a peer-requested read length to the serve buffer, failing
// ABOUTME: closed on a 32-bit int(uint32) negative wrap of the requested size.

package service

import "math"

// clampReadSize bounds a peer-requested read length to [0, bufLen]. On the 32-bit
// builds we ship (armeabi-v7a, x86), int(rec.Size) can wrap negative for a large
// uint32; a high-only "size > bufLen" bound misses that, and slicing buf[:size]
// with a negative size is out of range. Clamping the low end too fails closed.
func clampReadSize(size, bufLen int) int {
	if size < 0 || size > bufLen {
		return bufLen
	}
	return size
}

// safeReadOffset converts a peer-requested offset, reporting false when it cannot be
// represented. rec.Offset is uint64, so anything at or above 1<<63 lands negative in an
// int64 and ReadAt would only reject it after the handler has already opened the file.
func safeReadOffset(off uint64) (int64, bool) {
	if off > math.MaxInt64 {
		return 0, false
	}
	return int64(off), true
}
