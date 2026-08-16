// ABOUTME: Tests for the rekey ratchet KDF wrappers: determinism, domain separation
// ABOUTME: across epoch/direction/fold, CK!=MK independence, length, and validation.

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

func TestRatchetKeys_DeterministicAndDistinct(t *testing.T) {
	ck0 := testkit.RandBytes(t, KeySize)

	ck1, mk1, err := RatchetKeys(ck0, 0x01, 1, nil)
	require.NoError(t, err)
	require.Len(t, ck1, KeySize)
	require.Len(t, mk1, KeySize)

	// Deterministic: the same inputs derive the same keys (both peers must agree).
	ck1b, mk1b, err := RatchetKeys(ck0, 0x01, 1, nil)
	require.NoError(t, err)
	require.Equal(t, ck1, ck1b)
	require.Equal(t, mk1, mk1b)

	// CK and MK are independent: leaking the AEAD key must not reveal the chain.
	require.NotEqual(t, ck1, mk1)
	require.NotEqual(t, ck0, ck1)
}

func TestRatchetKeys_DomainSeparation(t *testing.T) {
	ck0 := testkit.RandBytes(t, KeySize)

	ck1, _, err := RatchetKeys(ck0, 0x01, 1, nil)
	require.NoError(t, err)

	ck2, _, err := RatchetKeys(ck0, 0x01, 2, nil)
	require.NoError(t, err)
	require.NotEqual(t, ck1, ck2)

	ckOtherDir, _, err := RatchetKeys(ck0, 0x02, 1, nil)
	require.NoError(t, err)
	require.NotEqual(t, ck1, ckOtherDir)
}

func TestRatchetKeys_FoldMixesEntropy(t *testing.T) {
	ck0 := testkit.RandBytes(t, KeySize)
	s1 := testkit.RandBytes(t, KeySize)
	s2 := testkit.RandBytes(t, KeySize)

	plainCK, plainMK, err := RatchetKeys(ck0, 0x01, 1, nil)
	require.NoError(t, err)

	foldCK, foldMK, err := RatchetKeys(ck0, 0x01, 1, s1)
	require.NoError(t, err)

	// A fold at the same epoch yields keys distinct from the plain ratchet, so plain vs folded candidates never collide.
	require.NotEqual(t, plainCK, foldCK)
	require.NotEqual(t, plainMK, foldMK)

	// The folded key depends on the staged secret: a different S derives different keys.
	foldCK2, _, err := RatchetKeys(ck0, 0x01, 1, s2)
	require.NoError(t, err)
	require.NotEqual(t, foldCK, foldCK2)

	// An empty (non-nil) fold secret is treated as no fold.
	emptyFoldCK, _, err := RatchetKeys(ck0, 0x01, 1, []byte{})
	require.NoError(t, err)
	require.Equal(t, plainCK, emptyFoldCK)
}

func TestRatchetKeys_RejectsShortChainKey(t *testing.T) {
	short := testkit.RandBytes(t, KeySize-1)
	_, _, err := RatchetKeys(short, 0x01, 1, nil)
	require.Error(t, err, "a chain key shorter than 32 bytes must be rejected")
}

func TestRatchetKeys_RejectsWrongLengthFoldSecret(t *testing.T) {
	ck0 := testkit.RandBytes(t, KeySize)
	_, _, err := RatchetKeys(ck0, 0x01, 1, testkit.RandBytes(t, 16))
	require.Error(t, err, "a fold secret that is not 32 bytes must be rejected")
}
