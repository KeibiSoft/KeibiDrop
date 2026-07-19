// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQUICControlChannel_ConnectsInFlow is the happy path for the wired QUIC control
// channel: two real peers connect over the full flow, the QUIC control pair comes up on
// both (keyed from the TCP handshake payload), and it carries a working control Ping —
// alongside the TCP file-transfer path, which the other integration tests exercise.
func TestQUICControlChannel_ConnectsInFlow(t *testing.T) {
	tp := SetupPeerPair(t, false)

	// The QUIC control channel dials in the background; both peers should connect.
	WaitForCondition(t, 15*time.Second, 100*time.Millisecond, func() bool {
		return tp.Alice.QUICControlConnected() && tp.Bob.QUICControlConnected()
	}, "QUIC control channel to connect on both peers")

	// It carries a working control RPC in each direction.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, tp.Alice.PingQUICControl(ctx), "alice ping over QUIC control channel")
	require.NoError(t, tp.Bob.PingQUICControl(ctx), "bob ping over QUIC control channel")
}
