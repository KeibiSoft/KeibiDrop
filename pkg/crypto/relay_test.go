// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package crypto

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDeriveRelayKeys_Integrity(t *testing.T) {
	roomPassword := make([]byte, 32)
	for i := range roomPassword {
		roomPassword[i] = 0xA5
	}

	lookup1, enc1, err := DeriveRelayKeys(roomPassword)
	require.NoError(t, err)
	
	lookup2, enc2, err := DeriveRelayKeys(roomPassword)
	require.NoError(t, err)

	// 1. Determinism: same input must produce same output
	assert.Equal(t, lookup1, lookup2)
	assert.Equal(t, enc1, enc2)

	// 2. Separation: lookup key must NOT be equal to encryption key
	assert.NotEqual(t, lookup1, enc1, "Lookup and encryption keys must be distinct")

	// 3. Different password must produce different keys
	roomPassword[0] ^= 0xFF
	lookup3, enc3, err := DeriveRelayKeys(roomPassword)
	require.NoError(t, err)
	assert.NotEqual(t, lookup1, lookup3)
	assert.NotEqual(t, enc1, enc3)
}
