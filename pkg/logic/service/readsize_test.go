// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests clampReadSize, which bounds a peer-requested read length
// ABOUTME: to the serve buffer and fails closed on a 32-bit negative wrap.

package service

import (
	"math"
	"testing"
)

func TestClampReadSize(t *testing.T) {
	cases := []struct{ size, bufLen, want int }{
		{10, 100, 10},             // normal
		{100, 100, 100},           // exact
		{200, 100, 100},           // oversized -> bufLen
		{0, 100, 0},               // zero
		{-1, 100, 100},            // negative wrap -> bufLen (the crash the finding is about)
		{math.MinInt32, 100, 100}, // int32 min, what int(large uint32) yields on 32-bit
	}
	for _, c := range cases {
		if got := clampReadSize(c.size, c.bufLen); got != c.want {
			t.Errorf("clampReadSize(%d, %d) = %d, want %d", c.size, c.bufLen, got, c.want)
		}
	}
}

func TestSafeReadOffset(t *testing.T) {
	cases := []struct {
		in   uint64
		want int64
		ok   bool
	}{
		{0, 0, true},
		{4096, 4096, true},
		{math.MaxInt64, math.MaxInt64, true},
		{1 << 63, 0, false},        // wraps negative through int64
		{math.MaxUint64, 0, false}, // the -1 case
	}
	for _, c := range cases {
		got, ok := safeReadOffset(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("safeReadOffset(%d) = (%d, %v), want (%d, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
