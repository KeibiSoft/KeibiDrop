// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/chacha20poly1305"
)

func randomBytes(t *testing.T, size int) []byte {
	b := make([]byte, size)
	_, err := rand.Read(b)
	require.NoError(t, err, "failed to generate random bytes")
	return b
}

func TestSymmetricEncryption(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, seedSize)
	plaintext := []byte("this is a secret message")

	ciphertext, err := Encrypt(kek, plaintext)
	req.NoError(err, "Encrypt failed")

	decrypted, err := Decrypt(kek, ciphertext)
	req.NoError(err, "Decrypt failed")

	req.Equal(plaintext, decrypted, "Decrypted plaintext does not match original")
}

func TestAsymmetricKeyExchange(t *testing.T) {
	req := require.New(t)

	// ML-KEM
	privAlice, pubAlice, err := GenerateMLKEMKeypair()
	req.NoError(err)

	seed1, ct1 := pubAlice.Encapsulate()
	ssAlice, err := privAlice.Decapsulate(ct1)
	req.NoError(err)
	req.Equal(seed1, ssAlice, "Kyber shared secrets do not match")
}

func TestHybridKeyDerivation(t *testing.T) {
	req := require.New(t)

	sharedX := randomBytes(t, seedSize)
	sharedKEM := randomBytes(t, seedSize)

	kek1, err := DeriveChaCha20Key(sharedX, sharedKEM)
	req.NoError(err, "DeriveChaCha20Key failed")

	kek2, err := DeriveChaCha20Key(sharedX, sharedKEM)
	req.NoError(err, "DeriveChaCha20Key failed")

	req.Equal(kek1, kek2, "KEK derivation is not deterministic")
}

func TestProtocolEndToEndStream(t *testing.T) {
	req := require.New(t)

	// Generate real key pairs for Alice and Bob
	alicePrivMLKEM, alicePubMLKEM, _ := GenerateMLKEMKeypair()
	alicePrivCurve, alicePubCurve, _ := GenerateX25519Keypair()

	bobPrivCurve, bobPubCurve, _ := GenerateX25519Keypair()

	// Bob encapsulates
	seedKEM, ctKEM := alicePubMLKEM.Encapsulate()
	seedCurve := randomBytes(t, seedSize)
	ctCurve, err := X25519Decapsulate(seedCurve, bobPrivCurve, alicePubCurve)
	req.NoError(err)

	// Alice decapsulates
	recoveredKEM, err := alicePrivMLKEM.Decapsulate(ctKEM)
	req.NoError(err)
	recoveredCurve, err := X25519Decapsulate(ctCurve, alicePrivCurve, bobPubCurve)
	req.NoError(err)

	// Derive KEKs
	kekBob, err := DeriveChaCha20Key(seedCurve, seedKEM)
	req.NoError(err)
	kekAlice, err := DeriveChaCha20Key(recoveredCurve, recoveredKEM)
	req.NoError(err)
	req.Equal(kekAlice, kekBob, "KEK mismatch")

	// Prepare 553 KiB of data
	src := make([]byte, 553*1024)
	_, err = rand.Read(src)
	req.NoError(err)

	// Encrypt with Bob's KEK
	ciphertext, err := Encrypt(kekBob, src)
	req.NoError(err)

	// Decrypt with Alice's KEK
	plaintext, err := Decrypt(kekAlice, ciphertext)
	req.NoError(err)
	req.Equal(src, plaintext, "Decrypted stream does not match original")
}

func TestEncryptWithAAD_RoundTrip(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, KeySize)
	plaintext := []byte("hello AAD world")
	aad := []byte{0xde, 0xad, 0xbe, 0xef}

	ct, err := EncryptWithAAD(kek, plaintext, aad)
	req.NoError(err)

	pt, err := DecryptWithAAD(kek, ct, aad)
	req.NoError(err)
	req.Equal(plaintext, pt)
}

func TestDecryptWithAAD_AADTampered(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, KeySize)
	plaintext := []byte("tamper test")
	aad := []byte{0x01, 0x02, 0x03}

	ct, err := EncryptWithAAD(kek, plaintext, aad)
	req.NoError(err)

	tamperedAAD := []byte{0x01, 0x02, 0x04}
	_, err = DecryptWithAAD(kek, ct, tamperedAAD)
	req.Error(err, "expected AEAD failure on tampered AAD")
}

func TestDecryptWithAAD_NilAADCompatibleWithEncrypt(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, KeySize)
	plaintext := []byte("nil aad compat")

	ct, err := Encrypt(kek, plaintext)
	req.NoError(err)

	pt, err := DecryptWithAAD(kek, ct, nil)
	req.NoError(err)
	req.Equal(plaintext, pt)
}

func TestEncrypt_NilAAD_BackCompat(t *testing.T) {
	req := require.New(t)

	// --- Golden vector: pin the wire format documented as [nonce | Seal(...)] ---
	var kek [KeySize]byte
	for i := range kek {
		kek[i] = byte(i + 1) // 0x01..0x20
	}
	var nonce [NonceSize]byte
	for i := range nonce {
		nonce[i] = 0x42
	}
	plaintext := []byte("keibidrop-aead-wire-format-golden")

	aead, err := chacha20poly1305.New(kek[:])
	req.NoError(err)
	expected := append(append([]byte{}, nonce[:]...), aead.Seal(nil, nonce[:], plaintext, nil)...)
	const expectedHex = "4242424242424242424242423936247ec4eac5790a9f8357393ff6fa42ad3fcd57fed7071e0c83bc3df0102760b989d97f0a703748f9d6e4c33f76c0ab"
	req.Equal(expectedHex, hex.EncodeToString(expected))

	got, err := Decrypt(kek[:], expected)
	req.NoError(err)
	req.Equal(plaintext, got)

	// --- Length invariant across plaintext sizes ---
	kek2 := randomBytes(t, KeySize)
	cases := []int{0, 1, 1024, 64 * 1024}
	for _, size := range cases {
		pt := make([]byte, size)
		if size > 0 {
			_, err := rand.Read(pt)
			req.NoError(err)
		}

		ct, err := Encrypt(kek2, pt)
		req.NoError(err)
		req.Equal(
			size+28,
			len(ct),
			"ciphertext length mismatch for plaintext size %d", size,
		)

		decrypted, err := Decrypt(kek2, ct)
		req.NoError(err)
		req.Equal(size, len(decrypted),
			"decrypted length mismatch for size %d", size)
		if size > 0 {
			req.Equal(pt, decrypted, "round-trip mismatch for size %d", size)
		}
	}
}

func TestEncryptWithAAD_EmptyAADEqualsNoAAD(t *testing.T) {
	req := require.New(t)
	kek := randomBytes(t, KeySize)
	plaintext := []byte("empty vs nil aad")

	for _, aad := range [][]byte{nil, {}} {
		ct, err := EncryptWithAAD(kek, plaintext, aad)
		req.NoError(err)

		pt, err := Decrypt(kek, ct)
		req.NoError(err)
		req.Equal(plaintext, pt,
			"EncryptWithAAD(aad=%v) should round-trip via Decrypt", aad)
	}
}

func TestDeriveFileEncryptionKey_Stable(t *testing.T) {
	req := require.New(t)
	master := randomBytes(t, 32)
	salt := randomBytes(t, 16)
	info := "keibidrop-identity-file-v2"

	k1, err := DeriveFileEncryptionKey(master, salt, info)
	req.NoError(err)

	k2, err := DeriveFileEncryptionKey(master, salt, info)
	req.NoError(err)

	req.Equal(k1, k2, "same inputs must produce the same key")
}

func TestDeriveFileEncryptionKey_DiffersBySalt(t *testing.T) {
	req := require.New(t)
	master := randomBytes(t, 32)
	salt1 := randomBytes(t, 16)
	salt2 := randomBytes(t, 16)
	info := "keibidrop-identity-file-v2"

	k1, err := DeriveFileEncryptionKey(master, salt1, info)
	req.NoError(err)

	k2, err := DeriveFileEncryptionKey(master, salt2, info)
	req.NoError(err)

	req.NotEqual(k1, k2, "different salts must produce different keys")
}

func TestDeriveFileEncryptionKey_DiffersByInfo(t *testing.T) {
	req := require.New(t)
	master := randomBytes(t, 32)
	salt := randomBytes(t, 16)

	k1, err := DeriveFileEncryptionKey(master, salt, "keibidrop-identity-file-v2")
	req.NoError(err)

	k2, err := DeriveFileEncryptionKey(master, salt, "keibidrop-contacts-file-v2")
	req.NoError(err)

	req.NotEqual(k1, k2, "different info strings must produce different keys")
}

func TestDeriveFileEncryptionKey_RejectsShortSalt(t *testing.T) {
	req := require.New(t)
	master := randomBytes(t, 32)
	shortSalt := randomBytes(t, 8)

	_, err := DeriveFileEncryptionKey(master, shortSalt, "info")
	req.Error(err, "must reject salt shorter than 16 bytes")
}

func TestDeriveFileEncryptionKey_RejectsEmptyMaster(t *testing.T) {
	req := require.New(t)
	salt := randomBytes(t, 16)

	_, err := DeriveFileEncryptionKey(nil, salt, "info")
	req.Error(err, "must reject empty master key")

	_, err = DeriveFileEncryptionKey([]byte{}, salt, "info")
	req.Error(err, "must reject zero-length master key")
}

func TestProtocolMimic(t *testing.T) {
	req := require.New(t)

	// Alice (responder)
	_, pubAliceMLKEM, _ := GenerateMLKEMKeypair()
	privAliceX, pubAliceX, _ := GenerateX25519Keypair()

	fpAlice, err := ProtocolFingerprintV0(map[string][]byte{
		"x25519": pubAliceX.Bytes(),
		"mlkem":  pubAliceMLKEM.Bytes(),
	})
	req.NoError(err, "Alice fingerprint generation failed")

	// Bob (initiator)
	seed := randomBytes(t, seedSize)
	ct, err := X25519Encapsulate(seed, privAliceX, pubAliceX)
	req.NoError(err)

	recovered, err := X25519Decapsulate(ct, privAliceX, pubAliceX)
	req.NoError(err)
	req.Equal(seed, recovered, "Decrypted seed mismatch")

	// Fingerprint check
	fpAliceCheck, err := ProtocolFingerprintV0(map[string][]byte{
		"x25519": pubAliceX.Bytes(),
		"mlkem":  pubAliceMLKEM.Bytes(),
	})
	req.NoError(err)
	req.Equal(fpAlice, fpAliceCheck, "Fingerprint mismatch")

	t.Logf("Fingerprint: %s", base64.RawURLEncoding.EncodeToString([]byte(fpAlice)))
}
