// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Argon2id passphrase KDF for the identity package.
// ABOUTME: Parameters follow OWASP Password Storage Cheat Sheet (baseline 2026).

package identity

import (
	"errors"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters per OWASP Password Storage Cheat Sheet (Argon2id
// baseline as of the plan date 2026-05-02). Recalibrate at major releases.
const (
	argon2idMemoryKiB = 64 * 1024 // 64 MiB
	argon2idTime      = 3
	argon2idThreads   = 4
	argon2idKeyLen    = 32
)

// PassphraseKDFID is the kdf_id byte written into the envelope header for
// passphrase-derived keys.
const PassphraseKDFID = KDFPassphrase

// DerivePassphraseKey derives a 32-byte key from passphrase and salt using
// Argon2id. salt must be at least 16 bytes; passphrase must be non-empty.
func DerivePassphraseKey(passphrase string, salt []byte) ([]byte, error) {
	if len(salt) < 16 {
		return nil, errors.New("salt too short (need >= 16 bytes)")
	}
	if len(passphrase) == 0 {
		return nil, errors.New("empty passphrase")
	}
	return argon2.IDKey(
		[]byte(passphrase),
		salt,
		argon2idTime,
		argon2idMemoryKiB,
		argon2idThreads,
		argon2idKeyLen,
	), nil
}
