// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package crypto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGenerateSeed_Success(t *testing.T) {
	seed, err := GenerateSeed()
	assert.NoError(t, err)
	assert.NotNil(t, seed)
	assert.Equal(t, seedSize, len(seed))
}

func TestGenerateSeed_Uniqueness(t *testing.T) {
	seed1, err1 := GenerateSeed()
	seed2, err2 := GenerateSeed()
	
	assert.NoError(t, err1)
	assert.NoError(t, err2)
	assert.NotEqual(t, seed1, seed2, "Consecutive seeds should not be identical")
}
