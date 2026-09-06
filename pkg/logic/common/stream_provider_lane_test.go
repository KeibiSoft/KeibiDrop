// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: On-demand reads ride the QUIC lane only when it is direct; a relayed
// ABOUTME: lane carries control only, so a big read cannot starve its heartbeat.

package common

import (
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestOpenStreamProvider_RelayedLaneIsControlOnly(t *testing.T) {
	s, err := session.InitSession(testkit.DiscardLogger(), 26002, 26001)
	require.NoError(t, err)
	tcp, err := grpc.NewClient("passthrough:///tcp", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer tcp.Close()
	quic, err := grpc.NewClient("passthrough:///quic", grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	defer quic.Close()
	s.GRPCClient = bindings.NewKeibiServiceClient(tcp)

	kd := newBareKD()
	kd.session = s
	kd.quicControlClient = quic

	kd.quicPeerAddr = relayAddrPrefix + "quic1"
	sp, ok := kd.openStreamProvider().(*ImplFileStreamProvider)
	require.True(t, ok)
	require.Nil(t, sp.fast, "a relayed lane must not carry on-demand reads")

	kd.quicPeerAddr = "[fd00::aa]:26400"
	sp, ok = kd.openStreamProvider().(*ImplFileStreamProvider)
	require.True(t, ok)
	require.NotNil(t, sp.fast, "a direct lane keeps the on-demand split")
}
