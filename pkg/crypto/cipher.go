// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"fmt"
	"runtime"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/sys/cpu"
)

// CipherSuite identifies an AEAD cipher for the encrypted connection.
type CipherSuite string

const (
	CipherChaCha20 CipherSuite = "chacha20-poly1305"
	CipherAES256   CipherSuite = "aes-256-gcm"
)

// HasHardwareAES returns true if the CPU has hardware AES acceleration.
// On x86 this is AES-NI; on ARM64 this is the ARMv8 AES extension.
func HasHardwareAES() bool {
	switch runtime.GOARCH {
	case "amd64":
		return cpu.X86.HasAES
	case "arm64":
		return cpu.ARM64.HasAES
	default:
		return false
	}
}

// SupportedCiphers returns the cipher suites this peer supports,
// ordered by preference (best first).
func SupportedCiphers() []CipherSuite {
	if HasHardwareAES() {
		return []CipherSuite{CipherAES256, CipherChaCha20}
	}
	return []CipherSuite{CipherChaCha20}
}

// NegotiateCipher picks the best cipher both peers support.
// Returns the first cipher from `local` that also appears in `remote`.
// Falls back to ChaCha20 if no intersection (should never happen).
func NegotiateCipher(local, remote []CipherSuite) CipherSuite {
	remoteSet := make(map[CipherSuite]bool, len(remote))
	for _, c := range remote {
		remoteSet[c] = true
	}
	for _, c := range local {
		if remoteSet[c] {
			return c
		}
	}
	return CipherChaCha20 // safe fallback
}

// NewAEAD creates an AEAD cipher for the given suite and key.
// Both AES-256-GCM and ChaCha20-Poly1305 use 32-byte keys, 12-byte nonces,
// and 16-byte auth tags — the wire format is identical.
func NewAEAD(suite CipherSuite, key []byte) (cipher.AEAD, error) {
	switch suite {
	case CipherAES256:
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, fmt.Errorf("aes.NewCipher: %w", err)
		}
		return cipher.NewGCM(block)
	case CipherChaCha20:
		return chacha20poly1305.New(key)
	default:
		return nil, fmt.Errorf("unknown cipher suite: %s", suite)
	}
}

// DeriveKey derives a symmetric key using the appropriate HKDF label for the cipher suite.
// Domain separation ensures the same input secrets produce different keys for different ciphers.
func DeriveKey(suite CipherSuite, sharedSecrets ...[]byte) ([]byte, error) {
	switch suite {
	case CipherAES256:
		return DeriveAES256Key(sharedSecrets...)
	case CipherChaCha20:
		return DeriveChaCha20Key(sharedSecrets...)
	default:
		return nil, fmt.Errorf("unknown cipher suite: %s", suite)
	}
}
