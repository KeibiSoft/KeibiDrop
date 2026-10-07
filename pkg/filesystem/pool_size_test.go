// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: Unit test for poolSizeForFile.
// ABOUTME: Small files must get one stream, large files the full pool.

package filesystem

import "testing"

func TestPoolSizeForFile(t *testing.T) {
	cases := []struct {
		name string
		size int64
		want int
	}{
		{"unknown size keeps full pool", 0, StreamPoolSize},
		{"negative size keeps full pool", -1, StreamPoolSize},
		{"one byte", 1, 1},
		{"small file 15 KiB", 15 * 1024, 1},
		{"exactly one block", ReadAheadBlock, 1},
		{"one block plus a byte", ReadAheadBlock + 1, 2},
		{"three blocks", 3 * ReadAheadBlock, 3},
		{"four blocks", 4 * ReadAheadBlock, StreamPoolSize},
		{"huge file capped", 1 << 40, StreamPoolSize},
	}
	for _, c := range cases {
		if got := poolSizeForFile(c.size); got != c.want {
			t.Errorf("%s: poolSizeForFile(%d) = %d, want %d", c.name, c.size, got, c.want)
		}
	}
}
