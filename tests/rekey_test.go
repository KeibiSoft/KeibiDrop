// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// generateTestKeyPairs creates two sets of keys (Alice and Bob) for testing.
// The caller must run inside testkit.Run or testkit.Within, which recover the
// panic Must raises on a key generation failure.
func generateTestKeyPairs() (*crypto.OwnKeys, *crypto.PeerKeys, *crypto.OwnKeys, *crypto.PeerKeys) {
	aliceKemPriv, aliceKemPub := testkit.Must2(crypto.GenerateMLKEMKeypair())
	aliceX25519Priv, aliceX25519Pub := testkit.Must2(crypto.GenerateX25519Keypair())

	bobKemPriv, bobKemPub := testkit.Must2(crypto.GenerateMLKEMKeypair())
	bobX25519Priv, bobX25519Pub := testkit.Must2(crypto.GenerateX25519Keypair())

	aliceOwn := &crypto.OwnKeys{
		MlKemPrivate:  aliceKemPriv,
		MlKemPublic:   aliceKemPub,
		X25519Private: aliceX25519Priv,
		X25519Public:  aliceX25519Pub,
	}

	alicePeer := &crypto.PeerKeys{
		MlKemPublic:  bobKemPub,
		X25519Public: bobX25519Pub,
	}

	bobOwn := &crypto.OwnKeys{
		MlKemPrivate:  bobKemPriv,
		MlKemPublic:   bobKemPub,
		X25519Private: bobX25519Priv,
		X25519Public:  bobX25519Pub,
	}

	bobPeer := &crypto.PeerKeys{
		MlKemPublic:  aliceKemPub,
		X25519Public: aliceX25519Pub,
	}

	return aliceOwn, alicePeer, bobOwn, bobPeer
}

// TestRekeyRequest_KeyDerivation tests that both parties derive the same key from a rekey request.
func TestRekeyRequest_KeyDerivation(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(10*time.Second, "rekey request key derivation", func() error {
			aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs()

			req, aliceNewKey := testkit.Must2(session.CreateRekeyRequest(aliceOwn, alicePeer, 1, crypto.CipherChaCha20))
			if err := fp.Len("alice new key", aliceNewKey, 32); err != nil {
				return err
			}

			resp, bobNewKey := testkit.Must2(session.ProcessRekeyRequest(req, bobOwn, bobPeer, crypto.CipherChaCha20))

			// Both keys must match for the same direction.
			return fp.All(
				fp.True("response is not nil", resp != nil),
				fp.BytesEqual("alice and bob derive the same key", aliceNewKey, bobNewKey),
			)
		})
	})
}

// TestRekeyResponse_KeyDerivation tests full rekey handshake.
func TestRekeyResponse_KeyDerivation(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(10*time.Second, "rekey response key derivation", func() error {
			aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs()

			req, _ := testkit.Must2(session.CreateRekeyRequest(aliceOwn, alicePeer, 1, crypto.CipherChaCha20))
			resp, _ := testkit.Must2(session.ProcessRekeyRequest(req, bobOwn, bobPeer, crypto.CipherChaCha20))
			newKey := testkit.Must(session.ProcessRekeyResponse(resp, aliceOwn, alicePeer, crypto.CipherChaCha20))

			return fp.Len("new key", newKey, 32)
		})
	})
}

// TestRekeyRequest_MissingSeeds tests error handling for malformed requests.
func TestRekeyRequest_MissingSeeds(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "rekey request missing seeds", func() error {
			_, _, bobOwn, bobPeer := generateTestKeyPairs()

			// Malformed request is missing the x25519 seed.
			badReq := &bindings.RekeyRequest{
				EncSeeds: map[string][]byte{
					"mlkem": make([]byte, 32),
				},
				Epoch: 1,
			}

			_, _, err := session.ProcessRekeyRequest(badReq, bobOwn, bobPeer, crypto.CipherChaCha20)
			return fp.WantErr("reject request missing x25519 seed", err)
		})
	})
}

// TestRekeyEpochIncreases tests that epoch values are monotonic.
func TestRekeyEpochIncreases(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(10*time.Second, "rekey epoch increases", func() error {
			aliceOwn, alicePeer, _, _ := generateTestKeyPairs()

			var prevEpoch uint64
			for i := 1; i <= 5; i++ {
				epoch := uint64(i)
				req, _ := testkit.Must2(session.CreateRekeyRequest(aliceOwn, alicePeer, epoch, crypto.CipherChaCha20))

				if i > 1 {
					if err := fp.True("epoch increases", req.Epoch > prevEpoch); err != nil {
						return err
					}
				}
				prevEpoch = req.Epoch
			}
			return nil
		})
	})
}

// TestMultipleRekeys tests that multiple consecutive rekeys work.
func TestMultipleRekeys(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(15*time.Second, "multiple rekeys", func() error {
			aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs()

			var prevAliceKey, prevBobKey []byte

			for i := 1; i <= 3; i++ {
				epoch := uint64(i)

				req, aliceKey := testkit.Must2(session.CreateRekeyRequest(aliceOwn, alicePeer, epoch, crypto.CipherChaCha20))
				_, bobKey := testkit.Must2(session.ProcessRekeyRequest(req, bobOwn, bobPeer, crypto.CipherChaCha20))

				if err := fp.True(fmt.Sprintf("rekey %d keys match", i), bytes.Equal(aliceKey, bobKey)); err != nil {
					return err
				}

				// Forward secrecy: a rekeyed key must differ from the previous epoch's key.
				if prevAliceKey != nil {
					if err := fp.False(fmt.Sprintf("rekey %d key same as previous", i), bytes.Equal(aliceKey, prevAliceKey)); err != nil {
						return err
					}
				}

				prevAliceKey = aliceKey
				prevBobKey = bobKey
			}

			return fp.True("final keys are set", prevAliceKey != nil && prevBobKey != nil)
		})
	})
}

// TestCompromisedKeyLimitedExposure tests forward secrecy property.
func TestCompromisedKeyLimitedExposure(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(10*time.Second, "compromised key limited exposure", func() error {
			aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs()

			req1, key1, _ := session.CreateRekeyRequest(aliceOwn, alicePeer, 1, crypto.CipherChaCha20)
			_, bobKey1, _ := session.ProcessRekeyRequest(req1, bobOwn, bobPeer, crypto.CipherChaCha20)

			req2, key2, _ := session.CreateRekeyRequest(aliceOwn, alicePeer, 2, crypto.CipherChaCha20)
			_, bobKey2, _ := session.ProcessRekeyRequest(req2, bobOwn, bobPeer, crypto.CipherChaCha20)

			if err := fp.All(
				fp.BytesEqual("epoch 1 keys match", key1, bobKey1),
				fp.BytesEqual("epoch 2 keys match", key2, bobKey2),
				fp.False("keys differ between epochs, forward secrecy", bytes.Equal(key1, key2)),
			); err != nil {
				return err
			}

			testData := []byte("secret message for epoch 1")
			encrypted1 := testkit.Must(crypto.Encrypt(key1, testData))

			// A later epoch's key must not decrypt an earlier epoch's data.
			_, err := crypto.Decrypt(key2, encrypted1)
			return fp.WantErr("decrypt with wrong epoch key", err)
		})
	})
}

// TestRekeyRequest_NilKeys tests error handling for nil keys.
func TestRekeyRequest_NilKeys(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "rekey request nil keys", func() error {
			_, _, err1 := session.CreateRekeyRequest(nil, &crypto.PeerKeys{}, 1, crypto.CipherChaCha20)

			aliceOwn, _, _, _ := generateTestKeyPairs()
			_, _, err2 := session.CreateRekeyRequest(aliceOwn, nil, 1, crypto.CipherChaCha20)

			return fp.All(
				fp.WantErr("nil own keys rejected", err1),
				fp.WantErr("nil peer keys rejected", err2),
			)
		})
	})
}
