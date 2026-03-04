// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package crypto

import (
	"bytes"
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 1. REPRO: Predictable Protocol Output on Weak Entropy
// ============================================================================
// Attacker Goal: Predict session keys and decrypt traffic.
// Scenario: A weak PRNG or a compromised entropy source returns a static 
// or predictable stream. Because KeibiDrop doesn't verify entropy quality 
// or handle RNG errors in GenerateSeed, the entire protocol becomes deterministic.
// ============================================================================

type staticReader struct{}

func (s *staticReader) Read(p []byte) (n int, err error) {
	for i := range p {
		p[i] = 0x42 // Static "random" data
	}
	return len(p), nil
}

func TestRepro_PredictableProtocol(t *testing.T) {
	// Setup: Mock the global RNG to return static data
	oldReader := rand.Reader
	rand.Reader = &staticReader{}
	defer func() { rand.Reader = oldReader }()

	// Part A: Generate Seed
	seed1 := GenerateSeed()
	seed2 := GenerateSeed()

	// IMPACT: Seeds are identical and predictable
	assert.Equal(t, seed1, seed2, "Seeds must be different but are identical due to weak entropy")
	assert.Equal(t, byte(0x42), seed1[0])

	// Part B: Protocol Handshake (X25519 Encapsulation)
	priv, pub, _ := GenerateX25519Keypair()
	
	ct1, _ := X25519Encapsulate(seed1, priv, pub)
	ct2, _ := X25519Encapsulate(seed1, priv, pub)

	// IMPACT: The ciphertext is now DETERMINISTIC. 
	// Because the salt is also pulled from the same weak RNG, 
	// the same seed + same keys = same ciphertext.
	// This allows an attacker to perform "offline dictionary attacks" or 
	// simply recognize repeated sessions.
	t.Logf("Ciphertext 1: %x", ct1[:16])
	t.Logf("Ciphertext 2: %x", ct2[:16])
	assert.Equal(t, ct1, ct2, "Protocol output is deterministic! Salt uniqueness failed.")
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
