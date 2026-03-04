// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package crypto

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// VERIFICATION: Lack of Stream Integrity (Chunk Reordering)
// ============================================================================
func TestVerifyFix_ChunkReorderingAttack(t *testing.T) {
	kek, _ := RandomBytes(KeySize)
	
	dataA := bytes.Repeat([]byte("A"), int(BlockSize))
	dataB := bytes.Repeat([]byte("B"), int(BlockSize))
	original := append(dataA, dataB...)

	var encrypted bytes.Buffer
	err := EncryptChunked(kek, bytes.NewReader(original), &encrypted, uint64(len(original)))
	require.NoError(t, err)

	chunkSize := int(BlockSize + EncOverhead)
	ct := encrypted.Bytes()
	require.Equal(t, 2*chunkSize, len(ct))

	c1 := ct[:chunkSize]
	c2 := ct[chunkSize:]
	
	swapped := make([]byte, 0, len(ct))
	swapped = append(swapped, c2...)
	swapped = append(swapped, c1...)

	var decrypted bytes.Buffer
	err = DecryptChunked(kek, bytes.NewReader(swapped), &decrypted, uint64(len(swapped)))

	// FIX VERIFIED: DecryptChunked must fail because AAD (chunk index) will not match.
	assert.Error(t, err, "DecryptChunked MUST fail when chunks are reordered")
	assert.Contains(t, err.Error(), "failed to decrypt chunk", "Error should point to decryption failure")
}

// ============================================================================
// VERIFICATION: X25519 Malleability (Bit-Flipping)
// ============================================================================
func TestVerifyFix_KeyMalleability(t *testing.T) {
	priv, pub, _ := GenerateX25519Keypair()
	seed := bytes.Repeat([]byte{0xAA}, seedSize)

	ct, err := X25519Encapsulate(seed, priv, pub)
	require.NoError(t, err)

	// Flip a bit in the encrypted part (AEAD payload)
	// ciphertext format: [salt(32) | payload(32+16)]
	ct[32] ^= 0x01 

	_, err = X25519Decapsulate(ct, priv, priv.PublicKey())
	
	// FIX VERIFIED: Decapsulation must fail integrity check.
	assert.Error(t, err, "X25519Decapsulate MUST fail when ciphertext is tampered")
	assert.Contains(t, err.Error(), "integrity check failed")
}
