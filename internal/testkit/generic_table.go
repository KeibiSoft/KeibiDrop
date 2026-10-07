// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.
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

// StdSizes is the size ladder repeated by 6 of the 10 size tables in
// tests/benchmark_test.go. Match a table by its ladder, not by line number.
//
// The other four are deliberately NOT covered. They measure different things
// and keep their own ladders:
//   - 10MB, 100MB, 600MB               netem profiles
//   - 1KB to 1GB, seven steps          chunk-size sweep
//   - 1KB, 10KB, 100KB, 1MB            mount latency
//   - a {name, count, size} triple     multi-file write
//
// Names must stay exactly as written. Subtest names depend on them.
//
// Do not mutate it. A caller that writes `sizes := testkit.StdSizes` shares the
// backing array with every other test in the binary. Copy it first if a test
// needs its own ladder.
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
