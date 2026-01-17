// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
)

// generateTestKeyPairs creates two sets of keys (Alice and Bob) for testing.
func generateTestKeyPairs(t *testing.T) (*crypto.OwnKeys, *crypto.PeerKeys, *crypto.OwnKeys, *crypto.PeerKeys) {
	t.Helper()

	// Alice's keys.
	aliceKemPriv, aliceKemPub, err := crypto.GenerateMLKEMKeypair()
	if err != nil {
		t.Fatalf("Failed to generate Alice ML-KEM keys: %v", err)
	}
	aliceX25519Priv, aliceX25519Pub, err := crypto.GenerateX25519Keypair()
	if err != nil {
		t.Fatalf("Failed to generate Alice X25519 keys: %v", err)
	}

	// Bob's keys.
	bobKemPriv, bobKemPub, err := crypto.GenerateMLKEMKeypair()
	if err != nil {
		t.Fatalf("Failed to generate Bob ML-KEM keys: %v", err)
	}
	bobX25519Priv, bobX25519Pub, err := crypto.GenerateX25519Keypair()
	if err != nil {
		t.Fatalf("Failed to generate Bob X25519 keys: %v", err)
	}

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
	timeout := time.After(10 * time.Second)
	done := make(chan bool)

	go func() {
		aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs(t)

		// Alice creates a rekey request.
		req, aliceNewKey, err := session.CreateRekeyRequest(aliceOwn, alicePeer, 1)
		if err != nil {
			t.Errorf("CreateRekeyRequest failed: %v", err)
			done <- false
			return
		}

		if len(aliceNewKey) != 32 {
			t.Errorf("Expected 32-byte key, got %d", len(aliceNewKey))
			done <- false
			return
		}

		// Bob processes the request.
		resp, bobNewKey, err := session.ProcessRekeyRequest(req, bobOwn, bobPeer)
		if err != nil {
			t.Errorf("ProcessRekeyRequest failed: %v", err)
			done <- false
			return
		}

		if resp == nil {
			t.Error("ProcessRekeyRequest returned nil response")
			done <- false
			return
		}

		// Both keys should match for the same direction.
		if !bytes.Equal(aliceNewKey, bobNewKey) {
			t.Error("Alice and Bob derived different keys")
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out - possible deadlock")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestRekeyResponse_KeyDerivation tests full rekey handshake.
func TestRekeyResponse_KeyDerivation(t *testing.T) {
	t.Parallel()
	timeout := time.After(10 * time.Second)
	done := make(chan bool)

	go func() {
		aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs(t)

		// Alice initiates rekey.
		req, _, err := session.CreateRekeyRequest(aliceOwn, alicePeer, 1)
		if err != nil {
			t.Errorf("CreateRekeyRequest failed: %v", err)
			done <- false
			return
		}

		// Bob processes request and creates response.
		resp, _, err := session.ProcessRekeyRequest(req, bobOwn, bobPeer)
		if err != nil {
			t.Errorf("ProcessRekeyRequest failed: %v", err)
			done <- false
			return
		}

		// Alice processes Bob's response.
		newKey, err := session.ProcessRekeyResponse(resp, aliceOwn, alicePeer)
		if err != nil {
			t.Errorf("ProcessRekeyResponse failed: %v", err)
			done <- false
			return
		}

		if len(newKey) != 32 {
			t.Errorf("Expected 32-byte key, got %d", len(newKey))
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out - possible deadlock")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestRekeyRequest_MissingSeeds tests error handling for malformed requests.
func TestRekeyRequest_MissingSeeds(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		_, _, bobOwn, bobPeer := generateTestKeyPairs(t)

		// Create malformed request missing x25519 seed.
		badReq := &bindings.RekeyRequest{
			EncSeeds: map[string][]byte{
				"mlkem": make([]byte, 32),
				// Missing "x25519"
			},
			Epoch: 1,
		}

		_, _, err := session.ProcessRekeyRequest(badReq, bobOwn, bobPeer)
		if err == nil {
			t.Error("Expected error for missing x25519 seed")
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestRekeyEpochIncreases tests that epoch values are monotonic.
func TestRekeyEpochIncreases(t *testing.T) {
	t.Parallel()
	timeout := time.After(10 * time.Second)
	done := make(chan bool)

	go func() {
		aliceOwn, alicePeer, _, _ := generateTestKeyPairs(t)

		var prevEpoch uint64
		for i := 1; i <= 5; i++ {
			epoch := uint64(i)
			req, _, err := session.CreateRekeyRequest(aliceOwn, alicePeer, epoch)
			if err != nil {
				t.Errorf("CreateRekeyRequest failed at epoch %d: %v", epoch, err)
				done <- false
				return
			}

			if req.Epoch <= prevEpoch && i > 1 {
				t.Errorf("Epoch should increase: prev=%d, current=%d", prevEpoch, req.Epoch)
				done <- false
				return
			}
			prevEpoch = req.Epoch
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestMultipleRekeys tests that multiple consecutive rekeys work.
func TestMultipleRekeys(t *testing.T) {
	t.Parallel()
	timeout := time.After(15 * time.Second)
	done := make(chan bool)

	go func() {
		aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs(t)

		var prevAliceKey, prevBobKey []byte

		for i := 1; i <= 3; i++ {
			epoch := uint64(i)

			// Alice initiates.
			req, aliceKey, err := session.CreateRekeyRequest(aliceOwn, alicePeer, epoch)
			if err != nil {
				t.Errorf("Rekey %d CreateRekeyRequest failed: %v", i, err)
				done <- false
				return
			}

			// Bob processes.
			resp, bobKey, err := session.ProcessRekeyRequest(req, bobOwn, bobPeer)
			if err != nil {
				t.Errorf("Rekey %d ProcessRekeyRequest failed: %v", i, err)
				done <- false
				return
			}

			// Keys should match.
			if !bytes.Equal(aliceKey, bobKey) {
				t.Errorf("Rekey %d: keys don't match", i)
				done <- false
				return
			}

			// Keys should differ from previous.
			if prevAliceKey != nil && bytes.Equal(aliceKey, prevAliceKey) {
				t.Errorf("Rekey %d: key same as previous (no forward secrecy)", i)
				done <- false
				return
			}

			prevAliceKey = aliceKey
			prevBobKey = bobKey
			_ = resp // used for response processing
		}

		// Verify we don't have nil keys.
		if prevAliceKey == nil || prevBobKey == nil {
			t.Error("Final keys are nil")
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out - possible deadlock in rekey chain")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestCompromisedKeyLimitedExposure tests forward secrecy property.
func TestCompromisedKeyLimitedExposure(t *testing.T) {
	t.Parallel()
	timeout := time.After(10 * time.Second)
	done := make(chan bool)

	go func() {
		aliceOwn, alicePeer, bobOwn, bobPeer := generateTestKeyPairs(t)

		// Generate key for epoch 1.
		req1, key1, _ := session.CreateRekeyRequest(aliceOwn, alicePeer, 1)
		_, bobKey1, _ := session.ProcessRekeyRequest(req1, bobOwn, bobPeer)

		// Generate key for epoch 2.
		req2, key2, _ := session.CreateRekeyRequest(aliceOwn, alicePeer, 2)
		_, bobKey2, _ := session.ProcessRekeyRequest(req2, bobOwn, bobPeer)

		// Verify keys match within each epoch.
		if !bytes.Equal(key1, bobKey1) {
			t.Error("Epoch 1 keys don't match")
			done <- false
			return
		}
		if !bytes.Equal(key2, bobKey2) {
			t.Error("Epoch 2 keys don't match")
			done <- false
			return
		}

		// Verify keys differ between epochs (forward secrecy).
		if bytes.Equal(key1, key2) {
			t.Error("Keys should differ between epochs for forward secrecy")
			done <- false
			return
		}

		// Encrypt data with key1.
		testData := []byte("secret message for epoch 1")
		encrypted1, err := crypto.Encrypt(key1, testData)
		if err != nil {
			t.Errorf("Encrypt with key1 failed: %v", err)
			done <- false
			return
		}

		// key2 should NOT decrypt data encrypted with key1.
		_, err = crypto.Decrypt(key2, encrypted1)
		if err == nil {
			t.Error("key2 should not decrypt data from key1 (forward secrecy violation)")
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

// TestRekeyRequest_NilKeys tests error handling for nil keys.
func TestRekeyRequest_NilKeys(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		// Test with nil OwnKeys.
		_, _, err := session.CreateRekeyRequest(nil, &crypto.PeerKeys{}, 1)
		if err == nil {
			t.Error("Expected error for nil OwnKeys")
			done <- false
			return
		}

		// Test with nil PeerKeys.
		aliceOwn, _, _, _ := generateTestKeyPairs(t)
		_, _, err = session.CreateRekeyRequest(aliceOwn, nil, 1)
		if err == nil {
			t.Error("Expected error for nil PeerKeys")
			done <- false
			return
		}

		done <- true
	}()

	select {
	case <-timeout:
		t.Fatal("Test timed out")
	case success := <-done:
		if !success {
			t.Fatal("Test failed")
		}
	}
}

