// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNoteLowDisk_EventPerCrossingAndOnTheStateLine(t *testing.T) {
	kd := &KeibiDrop{logger: quietLogger(), wallet: &TokenWallet{}}
	events := &[]string{}
	kd.OnEvent = func(e string) { *events = append(*events, e) }

	kd.noteLowDisk(true, 40<<20)
	require.Equal(t, []string{"disk_low:40"}, *events)
	require.True(t, kd.DiskLow())
	st := kd.SessionState()
	require.True(t, st.DiskLow)
	require.Equal(t, "Not connected, save disk almost full", st.Text)

	kd.noteLowDisk(false, 900<<20)
	require.Equal(t, []string{"disk_low:40", "disk_ok:"}, *events)
	st = kd.SessionState()
	require.False(t, st.DiskLow)
	require.Equal(t, "Not connected", st.Text)
}
