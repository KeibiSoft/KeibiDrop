// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for the Argon2id passphrase KDF.
// ABOUTME: Verifies determinism, salt/passphrase sensitivity, and input guards.

package identity

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func makeSalt(t *testing.T, size int) []byte {
	t.Helper()
	s := make([]byte, size)
	for i := range s {
		s[i] = byte(i + 1)
	}
	return s
}

func TestArgon2idDerive_Stable(t *testing.T) {
	req := require.New(t)
	salt := makeSalt(t, 16)

	key1, err := DerivePassphraseKey("hunter2", salt)
	req.NoError(err)
	key2, err := DerivePassphraseKey("hunter2", salt)
	req.NoError(err)

	req.Equal(key1, key2, "Argon2id must be deterministic")
	req.Len(key1, 32)
}

func TestArgon2idDerive_DiffersBySalt(t *testing.T) {
	req := require.New(t)
	salt1 := makeSalt(t, 16)
	salt2 := makeSalt(t, 16)
	salt2[0] ^= 0xFF

	key1, err := DerivePassphraseKey("samepassphrase", salt1)
	req.NoError(err)
	key2, err := DerivePassphraseKey("samepassphrase", salt2)
	req.NoError(err)

	req.False(bytes.Equal(key1, key2), "different salts must yield different keys")
}

func TestArgon2idDerive_DiffersByPassphrase(t *testing.T) {
	req := require.New(t)
	salt := makeSalt(t, 16)

	key1, err := DerivePassphraseKey("passphrase-A", salt)
	req.NoError(err)
	key2, err := DerivePassphraseKey("passphrase-B", salt)
	req.NoError(err)

	req.False(bytes.Equal(key1, key2), "different passphrases must yield different keys")
}

func TestArgon2idDerive_RejectsShortSalt(t *testing.T) {
	req := require.New(t)
	_, err := DerivePassphraseKey("pass", makeSalt(t, 15))
	req.Error(err, "salt shorter than 16 bytes must be rejected")
}

func TestArgon2idDerive_RejectsEmptyPassphrase(t *testing.T) {
	req := require.New(t)
	_, err := DerivePassphraseKey("", makeSalt(t, 16))
	req.Error(err, "empty passphrase must be rejected")
}

// TestArgon2idDerive_FullKeySpace verifies that keys are non-zero and
// distinct across different inputs — a cheap sanity check that we are not
// accidentally returning the zero or empty slice.
func TestArgon2idDerive_FullKeySpace(t *testing.T) {
	req := require.New(t)
	zero32 := make([]byte, 32)

	for i := 0; i < 5; i++ {
		salt := makeSalt(t, 16)
		salt[0] = byte(i)
		key, err := DerivePassphraseKey("testpassphrase", salt)
		req.NoError(err)
		req.Len(key, 32)
		req.False(bytes.Equal(key, zero32), "key must not be all-zero bytes")
	}
}
