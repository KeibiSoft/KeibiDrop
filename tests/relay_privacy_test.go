// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"encoding/base64"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// TestExtractRoomPassword_Valid tests extraction from valid fingerprint.
func TestExtractRoomPassword_Valid(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		// Generate real keypair and fingerprint.
		kemPriv, kemPub, err := crypto.GenerateMLKEMKeypair()
		if err != nil {
			t.Errorf("GenerateMLKEMKeypair failed: %v", err)
			done <- false
			return
		}
		x25519Priv, x25519Pub, err := crypto.GenerateX25519Keypair()
		if err != nil {
			t.Errorf("GenerateX25519Keypair failed: %v", err)
			done <- false
			return
		}

		ownKeys := &crypto.OwnKeys{
			MlKemPrivate:  kemPriv,
			MlKemPublic:   kemPub,
			X25519Private: x25519Priv,
			X25519Public:  x25519Pub,
		}

		fp, err := ownKeys.Fingerprint()
		if err != nil {
			t.Errorf("Fingerprint failed: %v", err)
			done <- false
			return
		}

		// Extract room password.
		roomPassword, err := crypto.ExtractRoomPassword(fp)
		if err != nil {
			t.Errorf("ExtractRoomPassword failed: %v", err)
			done <- false
			return
		}

		if len(roomPassword) != 32 {
			t.Errorf("Expected 32 bytes, got %d", len(roomPassword))
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

// TestExtractRoomPassword_TooShort tests that short fingerprints fail.
func TestExtractRoomPassword_TooShort(t *testing.T) {
	t.Parallel()
	timeout := time.After(2 * time.Second)
	done := make(chan bool)

	go func() {
		// Create a fingerprint that's too short (only 8 bytes when decoded).
		shortBytes := make([]byte, 8)
		shortFp := base64.RawURLEncoding.EncodeToString(shortBytes)

		_, err := crypto.ExtractRoomPassword(shortFp)
		if err == nil {
			t.Error("Expected error for short fingerprint, got nil")
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

// TestDeriveRelayKeys_Deterministic tests that same input produces same keys.
func TestDeriveRelayKeys_Deterministic(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		roomPassword := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

		lookup1, enc1, err := crypto.DeriveRelayKeys(roomPassword)
		if err != nil {
			t.Errorf("First DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		lookup2, enc2, err := crypto.DeriveRelayKeys(roomPassword)
		if err != nil {
			t.Errorf("Second DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		if !bytes.Equal(lookup1, lookup2) {
			t.Error("Lookup keys should be identical for same password")
			done <- false
			return
		}

		if !bytes.Equal(enc1, enc2) {
			t.Error("Encryption keys should be identical for same password")
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

// TestDeriveRelayKeys_Different tests that different passwords produce different keys.
func TestDeriveRelayKeys_Different(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		password1 := []byte("0123456789abcdef0123456789abcdef")
		password2 := []byte("fedcba9876543210fedcba9876543210")

		lookup1, enc1, err := crypto.DeriveRelayKeys(password1)
		if err != nil {
			t.Errorf("First DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		lookup2, enc2, err := crypto.DeriveRelayKeys(password2)
		if err != nil {
			t.Errorf("Second DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		if bytes.Equal(lookup1, lookup2) {
			t.Error("Lookup keys should differ for different passwords")
			done <- false
			return
		}

		if bytes.Equal(enc1, enc2) {
			t.Error("Encryption keys should differ for different passwords")
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

// TestDeriveRelayKeys_LookupNotEqualEncryption tests that lookup and encryption keys differ.
func TestDeriveRelayKeys_LookupNotEqualEncryption(t *testing.T) {
	t.Parallel()
	timeout := time.After(2 * time.Second)
	done := make(chan bool)

	go func() {
		roomPassword := []byte("0123456789abcdef0123456789abcdef")

		lookup, enc, err := crypto.DeriveRelayKeys(roomPassword)
		if err != nil {
			t.Errorf("DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		if bytes.Equal(lookup, enc) {
			t.Error("Lookup key and encryption key should not be equal")
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

// TestEncryptedRegistration_RoundTrip tests encrypt then decrypt preserves data.
func TestEncryptedRegistration_RoundTrip(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		roomPassword := []byte("0123456789abcdef0123456789abcdef")
		_, encKey, err := crypto.DeriveRelayKeys(roomPassword)
		if err != nil {
			t.Errorf("DeriveRelayKeys failed: %v", err)
			done <- false
			return
		}

		// Simulate registration data.
		originalData := []byte(`{"fingerprint":"abc123","listen":{"ip":"::1","port":26001}}`)

		// Encrypt.
		encrypted, err := crypto.Encrypt(encKey, originalData)
		if err != nil {
			t.Errorf("Encrypt failed: %v", err)
			done <- false
			return
		}

		// Decrypt.
		decrypted, err := crypto.Decrypt(encKey, encrypted)
		if err != nil {
			t.Errorf("Decrypt failed: %v", err)
			done <- false
			return
		}

		if !bytes.Equal(originalData, decrypted) {
			t.Errorf("Decrypted data mismatch: got %s, want %s", decrypted, originalData)
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

// TestEncryptedRegistration_WrongKey tests that wrong key fails decryption.
func TestEncryptedRegistration_WrongKey(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		password1 := []byte("0123456789abcdef0123456789abcdef")
		password2 := []byte("fedcba9876543210fedcba9876543210")

		_, encKey1, _ := crypto.DeriveRelayKeys(password1)
		_, encKey2, _ := crypto.DeriveRelayKeys(password2)

		originalData := []byte(`{"fingerprint":"secret"}`)

		// Encrypt with key1.
		encrypted, err := crypto.Encrypt(encKey1, originalData)
		if err != nil {
			t.Errorf("Encrypt failed: %v", err)
			done <- false
			return
		}

		// Try to decrypt with key2 (should fail).
		_, err = crypto.Decrypt(encKey2, encrypted)
		if err == nil {
			t.Error("Expected decryption to fail with wrong key")
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

// TestRelayCannotReverseEngineerFingerprint tests that relay cannot derive fingerprint from lookup key.
func TestRelayCannotReverseEngineerFingerprint(t *testing.T) {
	t.Parallel()
	timeout := time.After(5 * time.Second)
	done := make(chan bool)

	go func() {
		// Generate real keys.
		kemPriv, kemPub, _ := crypto.GenerateMLKEMKeypair()
		x25519Priv, x25519Pub, _ := crypto.GenerateX25519Keypair()

		ownKeys := &crypto.OwnKeys{
			MlKemPrivate:  kemPriv,
			MlKemPublic:   kemPub,
			X25519Private: x25519Priv,
			X25519Public:  x25519Pub,
		}

		fp, _ := ownKeys.Fingerprint()
		roomPassword, _ := crypto.ExtractRoomPassword(fp)
		lookupKey, _, _ := crypto.DeriveRelayKeys(roomPassword)

		// The lookup key is what the relay sees.
		// It should NOT contain the fingerprint or be reversible to it.
		fpBytes, _ := base64.RawURLEncoding.DecodeString(fp)

		// Check that lookup key doesn't contain fingerprint bytes.
		if bytes.Contains(lookupKey, fpBytes[:16]) {
			t.Error("Lookup key should not contain fingerprint bytes")
			done <- false
			return
		}

		// Key should be 32 bytes (ChaCha20 key size).
		if len(lookupKey) != 32 {
			t.Errorf("Lookup key should be 32 bytes, got %d", len(lookupKey))
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
