// ABOUTME: Tests for the ephemeral hybrid-KEM fold: round-trip agreement, 32-byte output,
// ABOUTME: salt binding, fresh-per-round entropy, tamper divergence, and input validation.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// runFold drives one full initiator/responder fold round and asserts both sides agree.
func runFold(t *testing.T, salt []byte) []byte {
	t.Helper()
	init, err := NewFoldInitiator()
	require.NoError(t, err)

	sResp, ct, respPub, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), salt)
	require.NoError(t, err)

	sInit, err := init.Derive(ct, respPub, salt)
	require.NoError(t, err)
	require.Equal(t, sResp, sInit, "initiator and responder must derive the same fold secret")
	return sInit
}

func TestEphemeralFold_RoundTripAgrees(t *testing.T) {
	salt := testkit.RandBytes(t, minFoldSaltSize)
	init, err := NewFoldInitiator()
	require.NoError(t, err)

	sResp, ct, respPub, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), salt)
	require.NoError(t, err)
	require.Len(t, sResp, KeySize)

	sInit, err := init.Derive(ct, respPub, salt)
	require.NoError(t, err)
	require.Len(t, sInit, KeySize)
	require.Equal(t, sResp, sInit)
}

func TestEphemeralFold_FreshPerRound(t *testing.T) {
	salt := testkit.RandBytes(t, minFoldSaltSize)
	require.NotEqual(t, runFold(t, salt), runFold(t, salt),
		"each round uses fresh ephemerals, so the fold secret must differ even under the same salt")
}

func TestEphemeralFold_SaltBinding(t *testing.T) {
	saltA := testkit.RandBytes(t, minFoldSaltSize)
	saltB := testkit.RandBytes(t, minFoldSaltSize)

	init, err := NewFoldInitiator()
	require.NoError(t, err)
	sResp, ct, respPub, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), saltA)
	require.NoError(t, err)

	// Same KEM material, different salt: the initiator must not agree with the responder.
	sInit, err := init.Derive(ct, respPub, saltB)
	require.NoError(t, err)
	require.NotEqual(t, sResp, sInit)
}

func TestEphemeralFold_TamperedCiphertextDiverges(t *testing.T) {
	salt := testkit.RandBytes(t, minFoldSaltSize)
	init, err := NewFoldInitiator()
	require.NoError(t, err)

	sResp, ct, respPub, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), salt)
	require.NoError(t, err)

	ct[0] ^= 0xFF // ML-KEM implicit rejection: no error, but a divergent shared secret
	sInit, err := init.Derive(ct, respPub, salt)
	require.NoError(t, err)
	require.NotEqual(t, sResp, sInit)
}

func TestEphemeralFold_MalformedPublicsRejected(t *testing.T) {
	salt := testkit.RandBytes(t, minFoldSaltSize)

	_, mlkemPub, err := GenerateMLKEMKeypair()
	require.NoError(t, err)
	_, x25519Pub, err := GenerateX25519Keypair()
	require.NoError(t, err)

	_, _, _, err = EphemeralFoldRespond([]byte("too-short"), x25519Pub.Bytes(), salt)
	require.Error(t, err)
	_, _, _, err = EphemeralFoldRespond(mlkemPub.Bytes(), []byte("too-short"), salt)
	require.Error(t, err)

	init, err := NewFoldInitiator()
	require.NoError(t, err)
	_, ct, _, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), salt)
	require.NoError(t, err)
	_, err = init.Derive(ct, []byte("too-short"), salt)
	require.Error(t, err)
}

func TestEphemeralFold_RejectsShortSalt(t *testing.T) {
	init, err := NewFoldInitiator()
	require.NoError(t, err)
	_, _, _, err = EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), testkit.RandBytes(t, minFoldSaltSize-1))
	require.Error(t, err, "responder rejects a short salt")

	init2, err := NewFoldInitiator()
	require.NoError(t, err)
	_, ct, respPub, err := EphemeralFoldRespond(init2.MLKEMPublic(), init2.X25519Public(), testkit.RandBytes(t, minFoldSaltSize))
	require.NoError(t, err)
	_, err = init2.Derive(ct, respPub, testkit.RandBytes(t, minFoldSaltSize-1))
	require.Error(t, err, "initiator rejects a short salt")
}

func TestEphemeralFold_DeriveConsumesInitiator(t *testing.T) {
	salt := testkit.RandBytes(t, minFoldSaltSize)
	init, err := NewFoldInitiator()
	require.NoError(t, err)
	_, ct, respPub, err := EphemeralFoldRespond(init.MLKEMPublic(), init.X25519Public(), salt)
	require.NoError(t, err)

	_, err = init.Derive(ct, respPub, salt)
	require.NoError(t, err)

	// Ephemeral privates are dropped after the first Derive; a second Derive must fail.
	_, err = init.Derive(ct, respPub, salt)
	require.Error(t, err)
}
