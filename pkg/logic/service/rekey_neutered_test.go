// ABOUTME: Guards that the in-band Rekey RPC is neutered: a (possibly forged) Rekey RPC cannot rotate or brick the live SecureConn.
// ABOUTME: The old handler swapped the connection key mid-stream; the relay could abuse that, so the RPC now fails open (Unimplemented).

//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package service

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRekeyRPCIsNeutered(t *testing.T) {
	key := make([]byte, kbc.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })

	inbound := session.NewSecureConn(c1, key, kbc.CipherAES256, session.NoncePrefixInbound)
	sess := &session.Session{Session: &session.SessionSockets{Inbound: inbound}}
	kd := &KeibidropServiceImpl{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: sess,
	}

	epochBefore := inbound.GetEpoch()
	resp, err := kd.Rekey(context.Background(), &bindings.RekeyRequest{Epoch: 99})

	require.Error(t, err)
	require.Nil(t, resp)
	require.Equal(t, codes.Unimplemented, status.Code(err), "a Rekey RPC must fail open, not swap the live key")
	require.Equal(t, epochBefore, inbound.GetEpoch(), "the Rekey RPC must not rotate (or brick) the live conn")
}
