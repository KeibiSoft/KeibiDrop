// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: One table runner, plus the payload-size table shared by benchmarks.
// ABOUTME: Subtest names come from the caller, so existing names are preserved.

package testkit

import "testing"

// NamedSize is the shape of the size table repeated across the benchmarks.
type NamedSize struct {
	Name string
	Size int
}

// StdSizes is the size ladder shared by 6 of the 9 tables in
// tests/benchmark_test.go, at lines 48, 476, 570, 644, 875 and 977.
//
// The other three tables are deliberately NOT covered here. They measure
// different things and must keep their own ladders:
//   - line 164: 10MB, 100MB, 600MB       (netem profiles)
//   - line 381: 1KB to 1GB, seven steps  (chunk-size sweep)
//   - line 1087: 1KB, 10KB, 100KB, 1MB   (mount latency)
//
// Names must stay exactly as written. Subtest names depend on them.
var StdSizes = []NamedSize{
	{"1MB", 1 * 1024 * 1024},
	{"10MB", 10 * 1024 * 1024},
	{"100MB", 100 * 1024 * 1024},
	{"1GB", 1024 * 1024 * 1024},
}

// RunTable runs one subtest per case.
// name supplies the subtest name, so a conversion keeps the original names.
// body returns an error, so a case composes with fp.All.
func RunTable[C any](t *testing.T, cases []C, name func(C) string, body func(*testing.T, C) error) {
	t.Helper()
	for _, c := range cases {
		t.Run(name(c), func(t *testing.T) {
			Run(t, func() error { return body(t, c) })
		})
	}
}

// RunTableB is RunTable for benchmarks.
// body runs inside b.Run. It must do its own timer control.
func RunTableB[C any](b *testing.B, cases []C, name func(C) string, body func(*testing.B, C)) {
	b.Helper()
	for _, c := range cases {
		b.Run(name(c), func(b *testing.B) { body(b, c) })
	}
}
