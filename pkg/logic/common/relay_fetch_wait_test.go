// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// A joiner polls the relay once a second until the creator registers, and
// every poll answered 404. Before 2026-09-27 each poll logged at ERROR
// ("Failed to fetch ... not found"), one line a second for as long as the
// person waited, because the HTTP helper mapped the 404 to ErrNotFound before
// the fetch reached its own quiet branch.
func TestGetRoomFromRelay_NotYetRegisteredIsAQuietWait(t *testing.T) {
	var polls atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(relay.Close)
	relayURL, err := url.Parse(relay.URL)
	require.NoError(t, err)

	var logs bytes.Buffer
	kd, err := NewKeibiDrop(t.Context(), testkit.Logger(&logs, slog.LevelDebug), false, relayURL, 0, 0, t.TempDir(), t.TempDir(), false, false)
	require.NoError(t, err)
	t.Cleanup(kd.Shutdown)
	kd.relayClient = relay.Client()

	fp, err := kd.ExportFingerprint()
	require.NoError(t, err)

	err = kd.getRoomFromRelay(fp)
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, int32(1), polls.Load(), "one poll, one 404")
	require.NotContains(t, logs.String(), "level=ERROR", "a peer that has not registered yet is not an error:\n%s", logs.String())
	require.Contains(t, logs.String(), "Not found", "the wait is still visible at debug level")
}
