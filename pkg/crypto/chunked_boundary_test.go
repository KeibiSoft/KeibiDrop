// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Boundary and edge-case tests for chunked symmetric encryption functions.
// ABOUTME: Covers EncryptedSize, DecryptedSize, EncryptChunked, and DecryptChunked at all critical size boundaries.

package crypto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptedSize_Boundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		plainSize uint64
		expected  uint64
	}{
		{
			name:      "empty",
			plainSize: 0,
			expected:  0,
		},
		{
			name:      "one byte",
			plainSize: 1,
			expected:  1 + EncOverhead,
		},
		{
			name:      "just under one block",
			plainSize: BlockSize - 1,
			expected:  BlockSize - 1 + EncOverhead,
		},
		{
			name:      "exactly one block",
			plainSize: BlockSize,
			expected:  BlockSize + EncOverhead,
		},
		{
			name:      "one block plus one byte",
			plainSize: BlockSize + 1,
			expected:  BlockSize + EncOverhead + 1 + EncOverhead,
		},
		{
			name:      "two full blocks",
			plainSize: 2 * BlockSize,
			expected:  2 * (BlockSize + EncOverhead),
		},
		{
			name:      "two blocks plus one byte",
			plainSize: 2*BlockSize + 1,
			expected:  2*(BlockSize+EncOverhead) + 1 + EncOverhead,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := EncryptedSize(tc.plainSize)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestDecryptedSize_Boundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		cipherSize    uint64
		expectedPlain uint64
		expectError   bool
	}{
		{
			name:        "zero — ciphertext too small",
			cipherSize:  0,
			expectError: true,
		},
		{
			name:        "EncOverhead minus one — ciphertext too small",
			cipherSize:  EncOverhead - 1,
			expectError: true,
		},
		{
			name:          "exactly EncOverhead — minimal valid empty plaintext",
			cipherSize:    EncOverhead,
			expectedPlain: 0,
			expectError:   false,
		},
		{
			name:          "EncOverhead plus one — one byte of plaintext",
			cipherSize:    EncOverhead + 1,
			expectedPlain: 1,
			expectError:   false,
		},
		{
			// fullChunks=1, remaining=1 → 1 < EncOverhead → incomplete final chunk.
			name:        "one full block plus one stray byte — incomplete final chunk",
			cipherSize:  BlockSize + EncOverhead + 1,
			expectError: true,
		},
		{
			name:          "one full encrypted block",
			cipherSize:    BlockSize + EncOverhead,
			expectedPlain: BlockSize,
			expectError:   false,
		},
		{
			// fullChunks=1, remaining=EncOverhead+1 → valid last chunk of 1 byte.
			name:          "one full block plus one byte of plaintext",
			cipherSize:    BlockSize + EncOverhead + EncOverhead + 1,
			expectedPlain: BlockSize + 1,
			expectError:   false,
		},
		{
			name:          "two full encrypted blocks",
			cipherSize:    2 * (BlockSize + EncOverhead),
			expectedPlain: 2 * BlockSize,
			expectError:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecryptedSize(tc.cipherSize)
			if tc.expectError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.expectedPlain, got)
			}
		})
	}
}

func TestEncryptChunked_DecryptChunked_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		plainSize uint64
	}{
		{name: "empty", plainSize: 0},
		{name: "one byte", plainSize: 1},
		{name: "just under one block", plainSize: BlockSize - 1},
		{name: "exactly one block", plainSize: BlockSize},
		{name: "one block plus one byte", plainSize: BlockSize + 1},
		{name: "two full blocks", plainSize: 2 * BlockSize},
		{name: "two blocks plus one byte", plainSize: 2*BlockSize + 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			kek := randomBytes(t, KeySize)

			var original []byte
			if tc.plainSize > 0 {
				original = randomBytes(t, int(tc.plainSize))
			}

			// Encrypt.
			var cipherBuf bytes.Buffer
			err := EncryptChunked(kek, bytes.NewReader(original), &cipherBuf, tc.plainSize)
			require.NoError(t, err, "EncryptChunked failed")

			// Verify the encrypted size matches the expected formula.
			expectedCipherSize := EncryptedSize(tc.plainSize)
			assert.Equal(t, int(expectedCipherSize), cipherBuf.Len(), "ciphertext size mismatch")

			// Decrypt.
			var plainBuf bytes.Buffer
			err = DecryptChunked(kek, &cipherBuf, &plainBuf, uint64(cipherBuf.Len()))
			require.NoError(t, err, "DecryptChunked failed")

			// Verify the recovered plaintext matches the original.
			assert.Equal(t, original, plainBuf.Bytes(), "decrypted plaintext does not match original")
		})
	}
}

func TestEncryptChunked_WrongKeyFails(t *testing.T) {
	t.Parallel()

	kekA := randomBytes(t, KeySize)
	kekB := randomBytes(t, KeySize)
	plaintext := randomBytes(t, int(BlockSize))

	var cipherBuf bytes.Buffer
	err := EncryptChunked(kekA, bytes.NewReader(plaintext), &cipherBuf, BlockSize)
	require.NoError(t, err, "EncryptChunked failed")

	var plainBuf bytes.Buffer
	err = DecryptChunked(kekB, &cipherBuf, &plainBuf, uint64(cipherBuf.Len()))
	require.Error(t, err, "DecryptChunked with wrong key must fail")
}

func TestEncryptChunked_TruncatedCiphertextFails(t *testing.T) {
	t.Parallel()

	kek := randomBytes(t, KeySize)
	// Use a 2-block payload to ensure we get a multi-chunk ciphertext.
	plainSize := 2 * BlockSize
	plaintext := randomBytes(t, int(plainSize))

	var cipherBuf bytes.Buffer
	err := EncryptChunked(kek, bytes.NewReader(plaintext), &cipherBuf, plainSize)
	require.NoError(t, err, "EncryptChunked failed")

	// Truncate by one byte.
	truncated := cipherBuf.Bytes()[:cipherBuf.Len()-1]
	truncatedSize := uint64(len(truncated))

	var plainBuf bytes.Buffer
	err = DecryptChunked(kek, bytes.NewReader(truncated), &plainBuf, truncatedSize)
	require.Error(t, err, "DecryptChunked on truncated ciphertext must fail")
}
