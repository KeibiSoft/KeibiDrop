// ABOUTME: Rekey ratchet key derivation: the forward CK/MK ratchet plus the KEM-
// ABOUTME: healing entropy fold, over HKDF-SHA512 with per-direction, per-epoch salts.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/sha512"
	"encoding/binary"
	"fmt"
)

const (
	labelRatchetCK     = "KeibiDrop-rekey-ratchet-CK-v1"
	labelRatchetMK     = "KeibiDrop-rekey-ratchet-MK-v1"
	labelRatchetCKFold = "KeibiDrop-rekey-ratchet-CK-fold-v1"
	labelRatchetMKFold = "KeibiDrop-rekey-ratchet-MK-fold-v1"
)

// ratchetSalt builds the per-direction, per-epoch HKDF salt: the 4-byte nonce prefix
// (direction) followed by the 2-byte big-endian epoch. It mirrors the epoch bytes the
// nonce carries, so the salt and the on-wire epoch always agree.
func ratchetSalt(prefix uint32, epoch uint16) []byte {
	salt := make([]byte, 6)
	binary.BigEndian.PutUint32(salt[:4], prefix)
	binary.BigEndian.PutUint16(salt[4:], epoch)
	return salt
}

// RatchetKeys derives the chaining key and message key for one epoch of a direction's
// ratchet from the previous chaining key, using HKDF-SHA512 with a per-direction,
// per-epoch salt and distinct info labels for CK and MK. The message key that goes
// into the AEAD is therefore independent of the chain secret that carries forward, so
// leaking the AEAD key does not reveal the chain.
//
// foldSecret is empty for the plain forward ratchet (forward secrecy rests on the
// previous CK being one-way and zeroized by the caller). To heal a compromise
// (post-compromise security), the caller passes the staged 32-byte KEM secret; it is
// mixed in as extra IKM under distinct fold labels, so a folded epoch never collides
// with the plain one and the reader can derive both and commit whichever authenticates.
func RatchetKeys(prevCK []byte, prefix uint32, epoch uint16, foldSecret []byte) (ck, mk []byte, err error) {
	// Fixed-length inputs keep the concatenated IKM unambiguous (no other split can
	// alias it) and fail closed on a malformed caller.
	if len(prevCK) != KeySize {
		return nil, nil, fmt.Errorf("chain key must be %d bytes, got %d", KeySize, len(prevCK))
	}
	if len(foldSecret) > 0 && len(foldSecret) != KeySize {
		return nil, nil, fmt.Errorf("fold secret must be %d bytes, got %d", KeySize, len(foldSecret))
	}

	salt := ratchetSalt(prefix, epoch)

	ckLabel, mkLabel := labelRatchetCK, labelRatchetMK
	secrets := [][]byte{prevCK}
	if len(foldSecret) > 0 {
		ckLabel, mkLabel = labelRatchetCKFold, labelRatchetMKFold
		secrets = [][]byte{prevCK, foldSecret}
	}

	ck, err = deriveKeyInternalWithSalt(sha512.New, salt, ckLabel, KeySize, secrets...)
	if err != nil {
		return nil, nil, err
	}
	mk, err = deriveKeyInternalWithSalt(sha512.New, salt, mkLabel, KeySize, secrets...)
	if err != nil {
		return nil, nil, err
	}
	return ck, mk, nil
}
