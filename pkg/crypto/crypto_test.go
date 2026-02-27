// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	ctCurve, err := X25519Encapsulate(seedCurve, bobPrivCurve, alicePubCurve)
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

// TestX25519EncapsulateDifferentOutputEachCall verifies that two calls with the same
// keys and same seed produce different ciphertexts (non-deterministic due to random salt).
func TestX25519EncapsulateDifferentOutputEachCall(t *testing.T) {
	privKey, pubKey, err := GenerateX25519Keypair()
	require.NoError(t, err)

	seed := randomBytes(t, seedSize)

	ct1, err := X25519Encapsulate(seed, privKey, pubKey)
	require.NoError(t, err)

	ct2, err := X25519Encapsulate(seed, privKey, pubKey)
	require.NoError(t, err)

	assert.NotEqual(t, ct1, ct2, "two encapsulations of the same seed must produce different ciphertexts")
}

// TestX25519RoundTrip_WithSalt verifies that encapsulate + decapsulate recovers the original seed.
func TestX25519RoundTrip_WithSalt(t *testing.T) {
	senderPriv, _, err := GenerateX25519Keypair()
	require.NoError(t, err)
	recipientPriv, recipientPub, err := GenerateX25519Keypair()
	require.NoError(t, err)
	senderPub := senderPriv.PublicKey()

	seed := randomBytes(t, seedSize)

	ct, err := X25519Encapsulate(seed, senderPriv, recipientPub)
	require.NoError(t, err)

	recovered, err := X25519Decapsulate(ct, recipientPriv, senderPub)
	require.NoError(t, err)

	assert.Equal(t, seed, recovered, "decapsulated seed must equal original seed")
}

// TestX25519KeystreamReuseAttackFails verifies that XOR-ing two ciphertexts no longer
// reveals the XOR of the two plaintexts (the attack that worked before the salt fix).
func TestX25519KeystreamReuseAttackFails(t *testing.T) {
	privKey, pubKey, err := GenerateX25519Keypair()
	require.NoError(t, err)

	seed1 := randomBytes(t, seedSize)
	seed2 := randomBytes(t, seedSize)

	ct1, err := X25519Encapsulate(seed1, privKey, pubKey)
	require.NoError(t, err)

	ct2, err := X25519Encapsulate(seed2, privKey, pubKey)
	require.NoError(t, err)

	// An attacker XORs the two 64-byte blobs and XORs with the known seed1,
	// hoping to recover seed2. With unique salts, the masks differ, so this fails.
	xorBlob := make([]byte, len(ct1))
	for i := range ct1 {
		xorBlob[i] = ct1[i] ^ ct2[i]
	}

	// Attempt the attack using only the ciphertext portions (bytes 32..64).
	attackGuess := make([]byte, seedSize)
	ct1Payload := ct1[32:]
	ct2Payload := ct2[32:]
	for i := range seedSize {
		attackGuess[i] = ct1Payload[i] ^ ct2Payload[i] ^ seed1[i]
	}

	assert.NotEqual(t, seed2, attackGuess, "keystream reuse attack must not recover seed2")
}
