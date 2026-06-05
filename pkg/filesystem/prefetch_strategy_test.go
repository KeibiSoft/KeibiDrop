// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package filesystem

import "testing"

// TestShouldPrefetchOnOpen pins the size-based strategy: small files stay pure
// on-demand, files >= the threshold prefetch (+ on-demand jumps), the explicit
// toggle forces any size, and a 0 threshold disables auto-prefetch entirely.
func TestShouldPrefetchOnOpen(t *testing.T) {
	const MB = 1024 * 1024
	cases := []struct {
		name           string
		prefetchOnOpen bool
		autoMB         int
		size           uint64
		want           bool
	}{
		{"50MB < 100MB default -> on-demand", false, 100, 50 * MB, false},
		{"just under 100MB -> on-demand", false, 100, 100*MB - 1, false},
		{"exactly 100MB -> prefetch", false, 100, 100 * MB, true},
		{"1.5GB -> prefetch", false, 100, 1536 * MB, true},
		{"force toggle prefetches a 1MB file", true, 100, 1 * MB, true},
		{"force toggle even with auto disabled", true, 0, 1 * MB, true},
		{"auto disabled (0) -> always on-demand", false, 0, 10000 * MB, false},
		{"custom 300MB threshold: 200MB on-demand", false, 300, 200 * MB, false},
		{"custom 300MB threshold: 400MB prefetch", false, 300, 400 * MB, true},
		{"zero-size file -> on-demand", false, 100, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldPrefetchOnOpen(c.prefetchOnOpen, c.autoMB, c.size); got != c.want {
				t.Errorf("shouldPrefetchOnOpen(prefetchOnOpen=%v, autoMB=%d, size=%d) = %v, want %v",
					c.prefetchOnOpen, c.autoMB, c.size, got, c.want)
			}
		})
	}
}
