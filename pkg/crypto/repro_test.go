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
// 1. REPRO: Unhandled RNG Error in Seed Generation
// ============================================================================
// Attacker Goal: Force the use of a predictable "zero-key".
// Scenario: A malicious actor on a shared server exhausts file descriptors or 
// entropy pool. The app continues with a seed of all zeros.
//
// NOTE: We test this by observing that GenerateSeed() has no way to return 
// an error, and looking at its source code reveals it ignores the error 
// from RandomBytes.
// ============================================================================

func TestRepro_PredictableSeedOnRNGFailure_Analysis(t *testing.T) {
	// Since we can't easily mock the package-level RandomBytes without 
	// changing the source code, we verify the IMPACT of the current 
	// implementation: it returns a value even if the underlying source 
	// were to fail (verified via manual code audit).
	
	t.Log("Audit check: pkg/crypto/utils.go:25 uses 'res, _ := RandomBytes(seedSize)'")
	t.Log("This means if RandomBytes fails, 'res' (all zeros) is returned silently.")
}

// ============================================================================
// 2. REPRO: Chunk Reordering / Silent Data Corruption
// ============================================================================
func TestRepro_ChunkReorderingAttack(t *testing.T) {
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

	if err == nil {
		t.Log("[!] VULNERABILITY CONFIRMED: DecryptChunked accepted swapped chunks.")
		assert.NotEqual(t, original, decrypted.Bytes(), "Data was silently rearranged")
	}
}

// ============================================================================
// 3. REPRO: X25519 Malleability (Bit-Flipping)
// ============================================================================
func TestRepro_KeyMalleability(t *testing.T) {
	priv, pub, _ := GenerateX25519Keypair()
	seed := bytes.Repeat([]byte{0xAA}, seedSize)

	ct, err := X25519Encapsulate(seed, priv, pub)
	require.NoError(t, err)

	// Flip the first bit of the payload part
	ct[32] ^= 0x80 

	recovered, err := X25519Decapsulate(ct, priv, priv.PublicKey())
	
	if err == nil {
		t.Log("[!] VULNERABILITY CONFIRMED: X25519Decapsulate returned a tampered seed without error.")
		assert.Equal(t, seed[0]^0x80, recovered[0], "The bit flip was perfectly preserved")
	}
}
