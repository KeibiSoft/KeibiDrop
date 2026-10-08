// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

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

// A joiner polls once a second until the creator registers, and each poll is a 404.
// That wait is not an error and must not log as one.
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
