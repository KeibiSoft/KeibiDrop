// ABOUTME: Guards the re-enabled Rekey RPC: it now drives the entropy fold via the session,
// ABOUTME: responding to a ready session and refusing cleanly (no rotation) when not ready.

//go:build !android

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package service

import (
	"context"
	"crypto/rand"
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

func randKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, kbc.KeySize)
	_, err := rand.Read(k)
	require.NoError(t, err)
	return k
}

func discardService(sess *session.Session) *KeibidropServiceImpl {
	return &KeibidropServiceImpl{
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		Session: sess,
	}
}

// A not-ready session (a half-built or forged target) must refuse the fold cleanly: the RPC
// is no longer Unimplemented, it returns a typed not-ready error, and it never rotates or
// bricks the live conn. This is the reversed contract of the old neutered handler.
func TestRekeyRPCRefusesWhenNotReady(t *testing.T) {
	key := randKey(t)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })

	inbound := session.NewSecureConn(c1, key, kbc.CipherAES256, session.NoncePrefixInbound)
	sess := &session.Session{Session: &session.SessionSockets{Inbound: inbound}}
	kd := discardService(sess)

	epochBefore := inbound.GetEpoch()
	resp, err := kd.Rekey(context.Background(), &bindings.RekeyRequest{Epoch: 99})

	require.Error(t, err)
	require.Nil(t, resp)
	require.NotEqual(t, codes.Unimplemented, status.Code(err), "the in-band Rekey path is re-enabled, not disabled")
	require.ErrorIs(t, err, session.ErrFoldNotReady, "an unprepared session refuses the fold cleanly")
	require.Equal(t, epochBefore, inbound.GetEpoch(), "a refused fold must not rotate or brick the live conn")
}

// A ready session answers a well-formed fold request: the handler wires straight to the
// session responder and returns the ML-KEM ciphertext plus the responder's ephemeral public.
func TestRekeyRPCRespondsToFold(t *testing.T) {
	key := randKey(t)
	suite := kbc.CipherAES256
	c1, c2 := net.Pipe()
	c3, c4 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close(); c3.Close(); c4.Close() })

	sess := &session.Session{
		SEKInbound:            randKey(t),
		SEKOutbound:           randKey(t),
		PeerSupportsKeyUpdate: true,
		Session: &session.SessionSockets{
			Inbound:  session.NewSecureConn(c1, key, suite, session.NoncePrefixInbound),
			Outbound: session.NewSecureConn(c3, key, suite, session.NoncePrefixOutbound),
		},
	}
	kd := discardService(sess)

	initiator, err := kbc.NewFoldInitiator()
	require.NoError(t, err)
	req := &bindings.RekeyRequest{EncSeeds: map[string][]byte{
		"mlkem":  initiator.MLKEMPublic(),
		"x25519": initiator.X25519Public(),
	}}

	resp, err := kd.Rekey(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotEmpty(t, resp.EncSeeds["ct"], "response carries the ml-kem ciphertext")
	require.NotEmpty(t, resp.EncSeeds["x25519"], "response carries the responder's ephemeral public")
}

// A nil session must not panic: the handler guards it and returns a status error.
func TestRekeyRPCNilSession(t *testing.T) {
	kd := discardService(nil)
	resp, err := kd.Rekey(context.Background(), &bindings.RekeyRequest{})
	require.Error(t, err)
	require.Nil(t, resp)
	require.NotEqual(t, codes.Unimplemented, status.Code(err))
}
