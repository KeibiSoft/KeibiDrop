// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

// ABOUTME: Tests that a read deadline on the conn makes an inbound handshake
// ABOUTME: fail fast instead of blocking forever on a silent peer.

package session

import (
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/stretchr/testify/require"
)

// A connected peer that never writes must not hang the inbound handshake: the handshake
// arms its own read deadline, so ReadFull fails fast instead of blocking forever. The
// bound lives inside PerformInboundHandshake, so no call site has to remember it.
func TestInboundHandshakeRespectsReadDeadline(t *testing.T) {
	shrinkHandshakeBounds(t, 200*time.Millisecond, 5*time.Second)
	bob, err := InitSession(testkit.DiscardLogger(), config.OutboundPort, config.InboundPort)
	require.NoError(t, err)

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	done := make(chan error, 1)
	go func() { done <- PerformInboundHandshake(bob, server) }()

	select {
	case err := <-done:
		require.Error(t, err, "a silent peer under a read deadline must fail the handshake")
	case <-time.After(5 * time.Second):
		t.Fatal("inbound handshake hung despite the read deadline")
	}
}
