// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

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
