// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// ABOUTME: Fuzz tests for the SecureReader wire-format parser.
// ABOUTME: Tests that malformed wire input never causes a panic.

package session

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// fuzzKey is a fixed 32-byte key used across all fuzz iterations.
// Using a package-level var avoids per-iteration key generation overhead.
var fuzzKey = func() []byte {
	k := make([]byte, kbc.KeySize)
	if _, err := rand.Read(k); err != nil {
		panic("failed to generate fuzz key: " + err.Error())
	}
	return k
}()

// validFramedMessage encrypts plaintext and returns a properly framed wire message:
// [4-byte big-endian uint32 length][encrypted payload].
func validFramedMessage(plaintext []byte) []byte {
	encrypted, err := kbc.Encrypt(fuzzKey, plaintext)
	if err != nil {
		panic("seed encryption failed: " + err.Error())
	}
	frame := make([]byte, 4+len(encrypted))
	//#nosec:G115 // safe cast; seed payloads are small.
	binary.BigEndian.PutUint32(frame[:4], uint32(len(encrypted)))
	copy(frame[4:], encrypted)
	return frame
}

// maxFuzzInputSize caps fuzzed input to avoid OOM from the uncapped allocation
// in SecureReader.Read(). The uncapped allocation is tracked in issue #50.
const maxFuzzInputSize = 64 * 1024 // 64 KiB

// corpusSeeds returns the canonical seed set used by both the fuzz function
// and the deterministic unit test.
func corpusSeeds() [][]byte {
	return [][]byte{
		// Valid complete message.
		validFramedMessage([]byte("hello KeibiDrop")),
		// Truncated: 3 bytes — incomplete length header (needs 4).
		{0x00, 0x00, 0x01},
		// Zero-length frame: length=0 followed by nothing.
		{0x00, 0x00, 0x00, 0x00},
		// Short payload: length=1, then 1 byte — too short for decryption (needs >=28 bytes).
		{0x00, 0x00, 0x00, 0x01, 0xFF},
		// Empty input.
		{},
	}
}

// FuzzSecureReader fuzzes the SecureReader wire-format parser.
// Invariant: no input must cause a panic. Errors are acceptable.
func FuzzSecureReader(f *testing.F) {
	for _, seed := range corpusSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// Skip oversized inputs to avoid OOM; uncapped allocation is tracked in issue #50.
		if len(data) > maxFuzzInputSize {
			return
		}

		// Also skip when the 4-byte length header declares a payload larger than our cap.
		// SecureReader.Read() calls make([]byte, length) without a bound check — that
		// uncapped allocation is the known issue tracked in #50. We guard here to prevent
		// CI OOM during fuzz runs.
		if len(data) >= 4 {
			declaredLen := binary.BigEndian.Uint32(data[:4])
			if declaredLen > maxFuzzInputSize {
				return
			}
		}

		sr := NewSecureReader(bytes.NewReader(data), fuzzKey)
		_, _ = sr.Read() // must not panic
	})
}

// TestSecureReader_CorpusSeeds runs the corpus seeds as deterministic unit tests.
// This exercises all error paths without requiring the -fuzz flag.
func TestSecureReader_CorpusSeeds(t *testing.T) {
	for _, seed := range corpusSeeds() {
		seed := seed // capture range variable
		t.Run("", func(t *testing.T) {
			sr := NewSecureReader(bytes.NewReader(seed), fuzzKey)
			_, _ = sr.Read() // must not panic; errors are expected for invalid inputs
		})
	}
}
