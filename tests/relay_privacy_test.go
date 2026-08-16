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

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// TestExtractRoomPassword_Valid tests extraction from valid fingerprint.
func TestExtractRoomPassword_Valid(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "extract room password", func() error {
			kemPriv, kemPub := testkit.Must2(crypto.GenerateMLKEMKeypair())
			x25519Priv, x25519Pub := testkit.Must2(crypto.GenerateX25519Keypair())

			ownKeys := &crypto.OwnKeys{
				MlKemPrivate:  kemPriv,
				MlKemPublic:   kemPub,
				X25519Private: x25519Priv,
				X25519Public:  x25519Pub,
			}

			roomPassword := testkit.Must(crypto.ExtractRoomPassword(testkit.Must(ownKeys.Fingerprint())))
			return fp.Len("room password", roomPassword, 32)
		})
	})
}

// TestExtractRoomPassword_TooShort tests that short fingerprints fail.
func TestExtractRoomPassword_TooShort(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(2*time.Second, "extract room password too short", func() error {
			// Fingerprint is too short at 8 bytes decoded.
			shortBytes := make([]byte, 8)
			shortFp := base64.RawURLEncoding.EncodeToString(shortBytes)

			_, err := crypto.ExtractRoomPassword(shortFp)
			return fp.WantErr("short fingerprint rejected", err)
		})
	})
}

// TestDeriveRelayKeys_Deterministic tests that same input produces same keys.
func TestDeriveRelayKeys_Deterministic(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "derive relay keys deterministic", func() error {
			roomPassword := []byte("0123456789abcdef0123456789abcdef") // 32 bytes

			lookup1, enc1 := testkit.Must2(crypto.DeriveRelayKeys(roomPassword))
			lookup2, enc2 := testkit.Must2(crypto.DeriveRelayKeys(roomPassword))

			return fp.All(
				fp.BytesEqual("lookup key for same password", lookup1, lookup2),
				fp.BytesEqual("encryption key for same password", enc1, enc2),
			)
		})
	})
}

// TestDeriveRelayKeys_Different tests that different passwords produce different keys.
func TestDeriveRelayKeys_Different(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "derive relay keys different", func() error {
			password1 := []byte("0123456789abcdef0123456789abcdef")
			password2 := []byte("fedcba9876543210fedcba9876543210")

			lookup1, enc1 := testkit.Must2(crypto.DeriveRelayKeys(password1))
			lookup2, enc2 := testkit.Must2(crypto.DeriveRelayKeys(password2))

			return fp.All(
				fp.False("lookup keys differ for different passwords", bytes.Equal(lookup1, lookup2)),
				fp.False("encryption keys differ for different passwords", bytes.Equal(enc1, enc2)),
			)
		})
	})
}

// TestDeriveRelayKeys_LookupNotEqualEncryption tests that lookup and encryption keys differ.
func TestDeriveRelayKeys_LookupNotEqualEncryption(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(2*time.Second, "relay keys lookup not equal encryption", func() error {
			roomPassword := []byte("0123456789abcdef0123456789abcdef")

			lookup, enc := testkit.Must2(crypto.DeriveRelayKeys(roomPassword))
			return fp.False("lookup key not equal encryption key", bytes.Equal(lookup, enc))
		})
	})
}

// TestEncryptedRegistration_RoundTrip tests encrypt then decrypt preserves data.
func TestEncryptedRegistration_RoundTrip(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "encrypted registration round trip", func() error {
			roomPassword := []byte("0123456789abcdef0123456789abcdef")
			_, encKey := testkit.Must2(crypto.DeriveRelayKeys(roomPassword))

			// Simulate registration data.
			originalData := []byte(`{"fingerprint":"abc123","listen":{"ip":"::1","port":26001}}`)

			encrypted := testkit.Must(crypto.Encrypt(encKey, originalData))
			decrypted := testkit.Must(crypto.Decrypt(encKey, encrypted))

			return fp.BytesEqual("decrypted data round trip", decrypted, originalData)
		})
	})
}

// TestEncryptedRegistration_WrongKey tests that wrong key fails decryption.
func TestEncryptedRegistration_WrongKey(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "encrypted registration wrong key", func() error {
			password1 := []byte("0123456789abcdef0123456789abcdef")
			password2 := []byte("fedcba9876543210fedcba9876543210")

			_, encKey1, _ := crypto.DeriveRelayKeys(password1)
			_, encKey2, _ := crypto.DeriveRelayKeys(password2)

			originalData := []byte(`{"fingerprint":"secret"}`)
			encrypted := testkit.Must(crypto.Encrypt(encKey1, originalData))

			_, err := crypto.Decrypt(encKey2, encrypted)
			return fp.WantErr("decrypt with wrong key", err)
		})
	})
}

// TestRelayCannotReverseEngineerFingerprint tests that relay cannot derive fingerprint from lookup key.
func TestRelayCannotReverseEngineerFingerprint(t *testing.T) {
	t.Parallel()
	testkit.Run(t, func() error {
		return testkit.Within(5*time.Second, "relay cannot reverse fingerprint", func() error {
			kemPriv, kemPub, _ := crypto.GenerateMLKEMKeypair()
			x25519Priv, x25519Pub, _ := crypto.GenerateX25519Keypair()

			ownKeys := &crypto.OwnKeys{
				MlKemPrivate:  kemPriv,
				MlKemPublic:   kemPub,
				X25519Private: x25519Priv,
				X25519Public:  x25519Pub,
			}

			fpr, _ := ownKeys.Fingerprint()
			roomPassword, _ := crypto.ExtractRoomPassword(fpr)
			lookupKey, _, _ := crypto.DeriveRelayKeys(roomPassword)

			// The lookup key is what the relay sees.
			// It must not contain the fingerprint or be reversible to it.
			fpBytes, _ := base64.RawURLEncoding.DecodeString(fpr)

			// 32 bytes is the ChaCha20 key size.
			return fp.All(
				fp.False("lookup key contains fingerprint bytes", bytes.Contains(lookupKey, fpBytes[:16])),
				fp.Len("lookup key", lookupKey, 32),
			)
		})
	})
}
