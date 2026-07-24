// ABOUTME: Tests for the session-level entropy fold: SEK-derived salt symmetry across peers
// ABOUTME: and a full initiator/responder exchange (no gRPC) that lands one shared secret.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// The two peers label the same key pair oppositely, so FoldSalt must sort the SEKs before
// deriving; differing salts would make the two sides derive different fold secrets.
func TestSession_FoldSaltSymmetry(t *testing.T) {
	sekX := randomBytes(t, kbc.KeySize)
	sekY := randomBytes(t, kbc.KeySize)

	a := &Session{SEKInbound: sekX, SEKOutbound: sekY}
	b := &Session{SEKInbound: sekY, SEKOutbound: sekX} // swapped: A.in == B.out, A.out == B.in

	saltA, err := a.FoldSalt()
	require.NoError(t, err)
	saltB, err := b.FoldSalt()
	require.NoError(t, err)

	require.Len(t, saltA, kbc.KeySize)
	require.Equal(t, saltA, saltB, "both peers must derive the same session-bound fold salt")
}

// FoldSalt fails closed when either SEK is absent.
func TestSession_FoldSaltRejectsEmptySEKs(t *testing.T) {
	_, err := (&Session{}).FoldSalt()
	require.Error(t, err, "no SEKs")

	_, err = (&Session{SEKInbound: randomBytes(t, kbc.KeySize)}).FoldSalt()
	require.Error(t, err, "outbound SEK missing")

	_, err = (&Session{SEKOutbound: randomBytes(t, kbc.KeySize)}).FoldSalt()
	require.Error(t, err, "inbound SEK missing")
}

// The fold initiator is the lexicographically lower fingerprint (same rule as reconnect).
func TestSession_IsFoldInitiator(t *testing.T) {
	lo := &Session{OwnFingerprint: "aaa", ExpectedPeerFingerprint: "bbb"}
	hi := &Session{OwnFingerprint: "bbb", ExpectedPeerFingerprint: "aaa"}
	require.True(t, lo.IsFoldInitiator(), "the lower fingerprint initiates")
	require.False(t, hi.IsFoldInitiator(), "the higher fingerprint responds")
}

// A full fold exchange without gRPC: the initiator's publics go to RespondToFold, it finishes
// with Derive, and both sides land on the same secret, staged on both responder conns.
func TestSession_FoldExchangeMatches(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sekX := randomBytes(t, kbc.KeySize)
	sekY := randomBytes(t, kbc.KeySize)

	// Responder: SEKs are the swapped pair, both conns stubbed, ratchet negotiated on.
	responder := &Session{
		SEKInbound:            sekY,
		SEKOutbound:           sekX,
		PeerSupportsKeyUpdate: true,
		Session: &SessionSockets{
			Inbound:  NewSecureConn(&writeSink{}, key, suite, NoncePrefixInbound),
			Outbound: NewSecureConn(&writeSink{}, key, suite, NoncePrefixOutbound),
		},
	}

	initiator, err := kbc.NewFoldInitiator()
	require.NoError(t, err)

	req := &bindings.RekeyRequest{EncSeeds: map[string][]byte{
		"mlkem":  initiator.MLKEMPublic(),
		"x25519": initiator.X25519Public(),
	}}
	resp, err := responder.RespondToFold(req)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.NotEmpty(t, resp.EncSeeds["ct"], "response carries the ml-kem ciphertext")
	require.NotEmpty(t, resp.EncSeeds["x25519"], "response carries the responder's ephemeral public")

	// The initiator derives with its own salt (the swapped SEK pair); it must match the
	// responder's sorted salt for the two secrets to agree.
	initSalt, err := (&Session{SEKInbound: sekX, SEKOutbound: sekY}).FoldSalt()
	require.NoError(t, err)
	initSecret, err := initiator.Derive(resp.EncSeeds["ct"], resp.EncSeeds["x25519"], initSalt)
	require.NoError(t, err)
	require.Len(t, initSecret, kbc.KeySize)

	// The responder staged its own derivation on both conns; it must equal the initiator's.
	require.Equal(t, initSecret, responder.Session.Outbound.stagedWriterFold,
		"initiator and responder must derive the same fold secret")
	require.Equal(t, initSecret, responder.Session.Inbound.stagedWriterFold,
		"the fold is staged on both conns of the peer")
}

// A fold round must stage its secret on the ExtraFoldConns (the QUIC control lane) too, nil
// entries skipped. Guards the responder-side hook, which skips the eager-fold driver.
func TestSession_FoldStagesExtraConns(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sekX := randomBytes(t, kbc.KeySize)
	sekY := randomBytes(t, kbc.KeySize)

	extraIn := NewSecureConn(&writeSink{}, key, suite, NoncePrefixInbound)
	extraOut := NewSecureConn(&writeSink{}, key, suite, NoncePrefixOutbound)
	responder := &Session{
		SEKInbound:            sekY,
		SEKOutbound:           sekX,
		PeerSupportsKeyUpdate: true,
		Session: &SessionSockets{
			Inbound:  NewSecureConn(&writeSink{}, key, suite, NoncePrefixInbound),
			Outbound: NewSecureConn(&writeSink{}, key, suite, NoncePrefixOutbound),
		},
		ExtraFoldConns: func() []*SecureConn { return []*SecureConn{extraIn, extraOut, nil} },
	}

	initiator, err := kbc.NewFoldInitiator()
	require.NoError(t, err)
	resp, err := responder.RespondToFold(&bindings.RekeyRequest{EncSeeds: map[string][]byte{
		"mlkem":  initiator.MLKEMPublic(),
		"x25519": initiator.X25519Public(),
	}})
	require.NoError(t, err)
	require.NotNil(t, resp)

	tcpSecret := responder.Session.Outbound.stagedWriterFold
	require.NotEmpty(t, tcpSecret)
	require.Equal(t, tcpSecret, extraIn.stagedWriterFold,
		"extra conn (inbound prefix) must be staged with the same fold secret")
	require.Equal(t, tcpSecret, extraOut.stagedWriterFold,
		"extra conn (outbound prefix) must be staged with the same fold secret")
}

// A successful responder fold emits one structured event="fold" line, so both peers surface
// the fold. The kd-bench harness keys on this marker.
func TestSession_RespondToFoldLogsCommit(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sekX := randomBytes(t, kbc.KeySize)
	sekY := randomBytes(t, kbc.KeySize)

	var buf bytes.Buffer
	responder := &Session{
		SEKInbound:            sekY,
		SEKOutbound:           sekX,
		PeerSupportsKeyUpdate: true,
		Session: &SessionSockets{
			Inbound:  NewSecureConn(&writeSink{}, key, suite, NoncePrefixInbound),
			Outbound: NewSecureConn(&writeSink{}, key, suite, NoncePrefixOutbound),
		},
		logger: slog.New(slog.NewTextHandler(&buf, nil)),
	}

	initiator, err := kbc.NewFoldInitiator()
	require.NoError(t, err)
	req := &bindings.RekeyRequest{EncSeeds: map[string][]byte{
		"mlkem":  initiator.MLKEMPublic(),
		"x25519": initiator.X25519Public(),
	}}
	_, err = responder.RespondToFold(req)
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, "event=fold",
		"the responder must emit a structured fold marker so both peers show a fold")
	require.Contains(t, out, "dir=respond",
		"the fold marker carries the responder side for the kd-bench parser")
	require.Contains(t, out, "epoch=",
		"the fold marker carries the ratchet epoch for the kd-bench parser")
}

// RespondToFold refuses when the session is not ready, so a stray or forged Rekey RPC cannot
// drive a fold on a half-built session.
func TestSession_RespondToFoldNotReady(t *testing.T) {
	_, err := (&Session{}).RespondToFold(&bindings.RekeyRequest{})
	require.ErrorIs(t, err, ErrFoldNotReady)
}

// InitiateFold needs a gRPC client; without one it fails fast instead of silently no-oping.
func TestSession_InitiateFoldRequiresClient(t *testing.T) {
	err := (&Session{}).InitiateFold(context.Background())
	require.ErrorIs(t, err, ErrFoldNoClient)
}

// The initiator (lower fingerprint) must refuse to answer a fold even when otherwise ready:
// only the responder answers, so a peer calling Rekey out of role cannot race a conflicting
// secret into the initiator's staged fold state.
func TestSession_RespondToFoldRejectsInitiatorRole(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	initiator := &Session{
		OwnFingerprint:          "aaa", // lower than the peer's, so this session is the initiator
		ExpectedPeerFingerprint: "bbb",
		SEKInbound:              randomBytes(t, kbc.KeySize),
		SEKOutbound:             randomBytes(t, kbc.KeySize),
		PeerSupportsKeyUpdate:   true,
		Session: &SessionSockets{
			Inbound:  NewSecureConn(&writeSink{}, key, suite, NoncePrefixInbound),
			Outbound: NewSecureConn(&writeSink{}, key, suite, NoncePrefixOutbound),
		},
	}
	fi, err := kbc.NewFoldInitiator()
	require.NoError(t, err)
	req := &bindings.RekeyRequest{EncSeeds: map[string][]byte{
		"mlkem": fi.MLKEMPublic(), "x25519": fi.X25519Public(),
	}}

	_, err = initiator.RespondToFold(req)
	require.ErrorIs(t, err, ErrFoldWrongRole, "the initiator refuses to answer a fold")
}
