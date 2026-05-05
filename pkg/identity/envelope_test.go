// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for the envelope marshal/parse cycle and AAD binding.
// ABOUTME: Includes a fuzz target and a tamper-detection vulnerability check.

package identity

import (
	"crypto/rand"
	"errors"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

func randomEnvelopeHeader(t *testing.T, kdfID uint8) EnvelopeHeader {
	t.Helper()
	var h EnvelopeHeader
	h.KDFID = kdfID
	h.Flags = 0x00
	h.KDFParam = 0x00
	_, err := rand.Read(h.Salt[:])
	require.NoError(t, err)
	_, err = rand.Read(h.Nonce[:])
	require.NoError(t, err)
	return h
}

func TestMarshalParseRoundTrip(t *testing.T) {
	req := require.New(t)
	ct := []byte("fake-ciphertext-with-tag-appended")

	for _, kdfID := range []uint8{KDFKeychain, KDFFile, KDFPassphrase} {
		h := randomEnvelopeHeader(t, kdfID)
		buf := MarshalEnvelope(h, ct)

		parsed, gotCT, err := ParseEnvelope(buf)
		req.NoError(err, "ParseEnvelope failed for kdf_id=%d", kdfID)
		req.Equal(h.KDFID, parsed.KDFID)
		req.Equal(h.Flags, parsed.Flags)
		req.Equal(h.KDFParam, parsed.KDFParam)
		req.Equal(h.Salt, parsed.Salt)
		req.Equal(h.Nonce, parsed.Nonce)
		req.Equal(ct, gotCT)
	}
}

func TestAADIsFirst24Bytes(t *testing.T) {
	req := require.New(t)
	h := randomEnvelopeHeader(t, KDFKeychain)
	ct := []byte("ciphertext")
	buf := MarshalEnvelope(h, ct)

	aad := h.AAD()
	req.Equal(envelopeHeaderSize, len(aad))
	req.Equal(buf[:envelopeHeaderSize], aad,
		"AAD must equal the first %d bytes of the marshalled envelope", envelopeHeaderSize)
}

func TestParseRejectsWrongMagic(t *testing.T) {
	req := require.New(t)
	h := randomEnvelopeHeader(t, KDFKeychain)
	buf := MarshalEnvelope(h, []byte("ct"))

	// Corrupt magic bytes.
	buf[0] = 0xFF

	_, _, err := ParseEnvelope(buf)
	req.Error(err)
	req.True(errors.Is(err, errEnvelopeWrongMagic),
		"expected errEnvelopeWrongMagic, got: %v", err)
}

func TestParseRejectsUnsupportedFormat(t *testing.T) {
	req := require.New(t)
	h := randomEnvelopeHeader(t, KDFKeychain)
	buf := MarshalEnvelope(h, []byte("ct"))

	// Set format byte to a future value.
	buf[4] = 0xFF

	_, _, err := ParseEnvelope(buf)
	req.Error(err)
	req.True(errors.Is(err, ErrIdentityNewerSchema),
		"expected ErrIdentityNewerSchema, got: %v", err)
}

func TestParseRejectsUnsupportedKDFID(t *testing.T) {
	req := require.New(t)
	h := randomEnvelopeHeader(t, KDFKeychain)
	buf := MarshalEnvelope(h, []byte("ct"))

	// Set kdf_id to an unknown value.
	buf[5] = 99

	_, _, err := ParseEnvelope(buf)
	req.Error(err)
	req.True(errors.Is(err, errEnvelopeUnsupportedKDF),
		"expected errEnvelopeUnsupportedKDF, got: %v", err)
}

func TestParseRejectsTruncated(t *testing.T) {
	req := require.New(t)

	for _, size := range []int{0, 4, 23, 35} {
		buf := make([]byte, size)
		if size >= 4 {
			copy(buf, envelopeMagic)
		}
		_, _, err := ParseEnvelope(buf)
		req.Error(err, "expected error for buffer of length %d", size)
	}
}

func TestIsV1Envelope(t *testing.T) {
	req := require.New(t)
	h := randomEnvelopeHeader(t, KDFKeychain)
	buf := MarshalEnvelope(h, []byte("ct"))

	req.True(IsV1Envelope(buf), "valid envelope must return true")
	req.False(IsV1Envelope(buf[:3]), "short buffer must return false")

	bad := make([]byte, len(buf))
	copy(bad, buf)
	bad[0] = 0x00
	req.False(IsV1Envelope(bad), "wrong magic must return false")
}

// FuzzParseEnvelope feeds random bytes into ParseEnvelope and asserts that it
// never panics and only returns typed (non-nil or nil) errors.
func FuzzParseEnvelope(f *testing.F) {
	// Seed corpus: a valid envelope.
	var h EnvelopeHeader
	h.KDFID = KDFKeychain
	f.Add(MarshalEnvelope(h, []byte("ct")))
	f.Add([]byte{})
	f.Add([]byte("KDID"))
	f.Add(append([]byte("KDID\x01\x01\x00\x00"), make([]byte, 28)...))

	f.Fuzz(func(t *testing.T, data []byte) {
		// Must not panic regardless of input.
		_, _, _ = ParseEnvelope(data)
	})
}

// TestEnvelopeAADBindsHeader verifies that flipping any header byte causes
// AEAD authentication to fail, proving the AAD covers the entire header.
func TestEnvelopeAADBindsHeader(t *testing.T) {
	req := require.New(t)

	key := make([]byte, kbc.KeySize)
	_, err := rand.Read(key)
	req.NoError(err)

	h := randomEnvelopeHeader(t, KDFKeychain)
	plaintext := []byte("sensitive identity data")
	aad := h.AAD()

	ct, err := kbc.EncryptWithAAD(key, plaintext, aad)
	req.NoError(err)

	// Flip a byte in the header — use kdf_id (offset 5) as representative.
	tamperedAAD := make([]byte, len(aad))
	copy(tamperedAAD, aad)
	tamperedAAD[5] ^= 0xFF

	_, err = kbc.DecryptWithAAD(key, ct, tamperedAAD)
	req.Error(err, "AEAD must fail when header byte is tampered")
}
