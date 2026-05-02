// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Envelope layout for identity and contacts files.
// ABOUTME: Provides marshal/parse helpers and AAD extraction for ChaCha20-Poly1305.

package identity

import (
	"errors"
	"fmt"
)

const (
	envelopeMagic      = "KDID"
	envelopeFormatV1   = 0x01
	envelopeHeaderSize = 24 // magic(4) + format(1) + kdf_id(1) + flags(1) + kdf_param(1) + salt(16)
	envelopeSaltSize   = 16
	envelopeNonceSize  = 12 // matches crypto.NonceSize (chacha20poly1305.NonceSize)
)

// KDF identifier constants stored in the envelope header.
const (
	KDFKeychain   uint8 = 1 // OS keychain (Tier 1a)
	KDFFile       uint8 = 2 // ~/.config/keibidrop/.master.key (Tier 1b)
	KDFPassphrase uint8 = 3 // Argon2id passphrase (Tier 2)
)

// maxKDFID is the highest kdf_id this version understands.
const maxKDFID uint8 = KDFPassphrase

// Sentinel errors for envelope parsing failures. These are internal; callers
// receive them wrapped via fmt.Errorf.
var (
	errEnvelopeTruncated      = errors.New("identity: envelope too short")
	errEnvelopeWrongMagic     = errors.New("identity: not a KeibiDrop identity file (wrong magic)")
	errEnvelopeUnsupportedKDF = errors.New("identity: unknown KDF identifier")
)

// EnvelopeHeader contains the decoded fields from the first 24 bytes of the
// envelope. Salt and Nonce are decoded separately; the raw header bytes live
// in the serialised form produced by MarshalEnvelope.
type EnvelopeHeader struct {
	KDFID    uint8
	Flags    uint8
	KDFParam uint8
	Salt     [envelopeSaltSize]byte
	Nonce    [envelopeNonceSize]byte
}

// MarshalEnvelope serialises header + ciphertextWithTag into the wire format:
//
//	[magic(4) | format(1) | kdf_id(1) | flags(1) | kdf_param(1) | salt(16) | nonce(12) | ct+tag]
func MarshalEnvelope(h EnvelopeHeader, ciphertextWithTag []byte) []byte {
	total := envelopeHeaderSize + envelopeNonceSize + len(ciphertextWithTag)
	buf := make([]byte, total)

	copy(buf[0:4], envelopeMagic)
	buf[4] = envelopeFormatV1
	buf[5] = h.KDFID
	buf[6] = h.Flags
	buf[7] = h.KDFParam
	copy(buf[8:8+envelopeSaltSize], h.Salt[:])
	copy(buf[envelopeHeaderSize:envelopeHeaderSize+envelopeNonceSize], h.Nonce[:])
	copy(buf[envelopeHeaderSize+envelopeNonceSize:], ciphertextWithTag)

	return buf
}

// ParseEnvelope decodes an envelope buffer into its header and ciphertext.
// Returns typed errors for invalid / unsupported envelopes.
func ParseEnvelope(buf []byte) (EnvelopeHeader, []byte, error) {
	minLen := envelopeHeaderSize + envelopeNonceSize
	if len(buf) < minLen {
		return EnvelopeHeader{}, nil, errEnvelopeTruncated
	}

	if string(buf[0:4]) != envelopeMagic {
		return EnvelopeHeader{}, nil, errEnvelopeWrongMagic
	}

	format := buf[4]
	if format > envelopeFormatV1 {
		return EnvelopeHeader{}, nil, fmt.Errorf("%w (got 0x%02x)", ErrIdentityNewerSchema, format)
	}

	kdfID := buf[5]
	if kdfID < KDFKeychain || kdfID > maxKDFID {
		return EnvelopeHeader{}, nil, fmt.Errorf("%w (kdf_id=%d)", errEnvelopeUnsupportedKDF, kdfID)
	}

	var h EnvelopeHeader
	h.KDFID = kdfID
	h.Flags = buf[6]
	h.KDFParam = buf[7]
	copy(h.Salt[:], buf[8:8+envelopeSaltSize])
	copy(h.Nonce[:], buf[envelopeHeaderSize:envelopeHeaderSize+envelopeNonceSize])

	ct := buf[envelopeHeaderSize+envelopeNonceSize:]
	return h, ct, nil
}

// AAD returns the first 24 bytes of the envelope (magic through salt) that
// must be passed as associated data to EncryptWithAAD / DecryptWithAAD.
// Tampering with any header byte invalidates the AEAD tag.
func (h EnvelopeHeader) AAD() []byte {
	buf := make([]byte, envelopeHeaderSize)
	copy(buf[0:4], envelopeMagic)
	buf[4] = envelopeFormatV1
	buf[5] = h.KDFID
	buf[6] = h.Flags
	buf[7] = h.KDFParam
	copy(buf[8:8+envelopeSaltSize], h.Salt[:])
	return buf
}

// IsV1Envelope returns true when buf starts with the correct magic bytes and
// is long enough to contain a complete header + nonce.
func IsV1Envelope(buf []byte) bool {
	if len(buf) < envelopeHeaderSize+envelopeNonceSize {
		return false
	}
	return string(buf[0:4]) == envelopeMagic
}
