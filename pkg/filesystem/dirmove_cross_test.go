// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package filesystem

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The crossing predicate: mv A B/ against mv B A/ must be detected in both
// orientations, same-source rename-rename too, and unrelated moves never.
func TestCrossingDirMove(t *testing.T) {
	d := newTestDir(t.TempDir())
	d.RecordLocalDirMove("/A", "/B/A")
	crossed, ourNew, ourOld := d.CrossingDirMove("/B", "/A/B")
	require.True(t, crossed, "mutual nesting must be detected")
	require.Equal(t, "/B/A", ourNew)
	require.Equal(t, "/A", ourOld)

	d2 := newTestDir(t.TempDir())
	d2.RecordLocalDirMove("/B", "/A/B")
	c2, _, _ := d2.CrossingDirMove("/A", "/B/A")
	require.True(t, c2, "the reverse orientation must be detected")

	d3 := newTestDir(t.TempDir())
	d3.RecordLocalDirMove("/X", "/Y")
	c3, _, _ := d3.CrossingDirMove("/X", "/Z")
	require.True(t, c3, "same-source rename-rename must be detected")

	d4 := newTestDir(t.TempDir())
	d4.RecordLocalDirMove("/A", "/B/A")
	c4, _, _ := d4.CrossingDirMove("/C", "/D/C")
	require.False(t, c4, "unrelated moves must not cross")

	d5 := newTestDir(t.TempDir())
	c5, _, _ := d5.CrossingDirMove("/B", "/A/B")
	require.False(t, c5, "no recorded local move, no crossing")
}
