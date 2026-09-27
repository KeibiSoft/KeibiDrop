// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// A relay that accepts registrations and never finds a peer: every connect verb waits on it.
func newWaitingRelayKD(t *testing.T) *common.KeibiDrop {
	t.Helper()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(relay.Close)
	relayURL, err := url.Parse(relay.URL)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := nextTestPort()
	kd, err := common.NewKeibiDropWithIP(ctx, slog.New(slog.NewTextHandler(os.Stderr, nil)), false, relayURL, port, port+1, "", t.TempDir(), false, false, "::1")
	require.NoError(t, err)
	kd.BridgeAddr = "127.0.0.1:9"
	return kd
}

// --timeout caps join and create, as it caps connect-timeout; before, the plain
// verbs ignored the flag and waited the daemon's own ten minutes.
func TestJoinAndCreate_HonorTheTimeoutFlag(t *testing.T) {
	kd := newWaitingRelayKD(t)
	peer := newTestKD(t)
	peerFP, err := peer.ExportFingerprint()
	require.NoError(t, err)
	require.True(t, dispatchTest(kd, "register", peerFP).OK)

	for _, verb := range []string{"join", "create"} {
		start := time.Now()
		resp := dispatch(kd, Request{Command: verb, TimeoutS: 1}, func() {}, nil)
		require.False(t, resp.OK, verb)
		require.Equal(t, codeTimeout, resp.Code, "%s: %s", verb, resp.Error)
		require.Less(t, time.Since(start), 10*time.Second, verb)
	}
}
