// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

const KeySize = chacha20poly1305.KeySize
const NonceSize = chacha20poly1305.NonceSize
const EncOverhead = uint64(chacha20poly1305.NonceSize + chacha20poly1305.Overhead)
const BlockSize = uint64(2 << 16) // On linux cp works with blocks of 128KiB, we use double.

// NonceGenerator provides deterministic counter-based nonces.
// Structure: [4-byte prefix][8-byte counter] = 12 bytes total.
// The prefix distinguishes directions (inbound vs outbound) to prevent reuse.
type NonceGenerator struct {
	prefix  [4]byte       // Direction/session identifier.
	counter atomic.Uint64 // Monotonic counter.
}

// NewNonceGenerator creates a nonce generator with the given prefix.
// Use different prefixes for inbound vs outbound to avoid nonce reuse.
func NewNonceGenerator(prefix uint32) *NonceGenerator {
	ng := &NonceGenerator{}
	binary.BigEndian.PutUint32(ng.prefix[:], prefix)
	return ng
}

// Next returns the next nonce and increments the counter.
// Thread-safe via atomic operations.
func (ng *NonceGenerator) Next() [NonceSize]byte {
	var nonce [NonceSize]byte
	copy(nonce[:4], ng.prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], ng.counter.Add(1))
	return nonce
}

// Count returns the current counter value (for monitoring/debugging).
func (ng *NonceGenerator) Count() uint64 {
	return ng.counter.Load()
}

// EncryptWithNonceAndAAD encrypts using a provided nonce and additional authenticated data.
// Returns [nonce | ciphertext+MAC], or error.
func EncryptWithNonceAndAAD(kek, plainText, aad []byte, nonce [NonceSize]byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	cipherText := aead.Seal(nil, nonce[:], plainText, aad)

	result := make([]byte, NonceSize+len(cipherText))
	copy(result, nonce[:])
	copy(result[NonceSize:], cipherText)

	return result, nil
}

// DecryptWithNonceAndAAD decrypts [nonce | ciphertext+MAC] using KEK and verifies AAD.
// Returns plainText or error if authentication fails.
func DecryptWithNonceAndAAD(kek, input, aad []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	if uint64(len(input)) < EncOverhead {
		return nil, errors.New("input too short")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	nonce := input[:chacha20poly1305.NonceSize]
	cipherText := input[chacha20poly1305.NonceSize:]

	plainText, err := aead.Open(nil, nonce, cipherText, aad)
	if err != nil {
		return nil, err
	}

	return plainText, nil
}

// EncryptWithNonce encrypts using a provided nonce (for counter-based encryption).
// Returns [nonce | ciphertext+MAC], or error.
func EncryptWithNonce(kek, plainText []byte, nonce [NonceSize]byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	cipherText := aead.Seal(nil, nonce[:], plainText, nil)

	result := make([]byte, NonceSize+len(cipherText))
	copy(result, nonce[:])
	copy(result[NonceSize:], cipherText)

	return result, nil
}

// Encrypt encrypts plainText using KEK with ChaCha20-Poly1305.
// Returns [nonce | ciphertext+MAC], or error.
func Encrypt(kek, plainText []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// The nonce is not hardcoded, I just generated it in the previous line.
	cipherText := aead.Seal(nil, nonce, plainText, nil) // #nosec G407

	result := make([]byte, len(nonce)+len(cipherText))
	copy(result, nonce)
	copy(result[chacha20poly1305.NonceSize:], cipherText)

	return result, nil
}

// Decrypt decrypts [nonce | ciphertext+MAC] using KEK.
// Returns plainText or error if authentication fails.
func Decrypt(kek, input []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	if uint64(len(input)) < EncOverhead {
		return nil, errors.New("input too short")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	nonce := input[:chacha20poly1305.NonceSize]
	cipherText := input[chacha20poly1305.NonceSize:]

	plainText, err := aead.Open(nil, nonce, cipherText, nil)
	if err != nil {
		return nil, err
	}

	return plainText, nil
}

func EncryptedSize(plainSize uint64) uint64 {
	fullChunks := plainSize / BlockSize
	lastChunkSize := plainSize % BlockSize
	cipherSize := fullChunks * (BlockSize + EncOverhead)

	if lastChunkSize > 0 {
		cipherSize += lastChunkSize + EncOverhead
	}

	return cipherSize
}

func DecryptedSize(cipherSize uint64) (uint64, error) {
	if cipherSize < EncOverhead {
		return 0, errors.New("ciphertext too small")
	}

	chunkWithOverhead := BlockSize + EncOverhead
	fullChunks := cipherSize / uint64(chunkWithOverhead)
	remaining := cipherSize % uint64(chunkWithOverhead)

	if remaining > 0 {
		if remaining < EncOverhead {
			return 0, errors.New("incomplete final chunk")
		}
		lastChunkSize := remaining - EncOverhead
		return fullChunks*BlockSize + lastChunkSize, nil
	}

	return fullChunks * BlockSize, nil
}

func EncryptChunked(kek []byte, r io.Reader, w io.Writer, plainSize uint64) error {
	buf := make([]byte, BlockSize)
	var totalRead uint64
	// Use a zero prefix for file chunks - separation is handled by KEK uniqueness per session
	ng := NewNonceGenerator(0)
	var chunkIdx uint64

	for totalRead < plainSize {
		toRead := BlockSize
		remaining := plainSize - totalRead
		if remaining < uint64(toRead) {
			toRead = remaining
		}

		n, err := io.ReadFull(r, buf[:toRead])
		if err != nil && err != io.EOF {
			return err
		}

		if n == 0 {
			break
		}

		//#nosec:G115 // n comes from io.ReadFull and will never be negative.
		totalRead += uint64(n)

		nonce := ng.Next()
		// Bind the chunk index to the ciphertext via AAD
		aad := make([]byte, 8)
		binary.BigEndian.PutUint64(aad, chunkIdx)

		encryptedChunk, err := EncryptWithNonceAndAAD(kek, buf[:n], aad, nonce)
		if err != nil {
			return err
		}

		if _, err := w.Write(encryptedChunk); err != nil {
			return err
		}
		chunkIdx++
	}

	return nil
}

func DecryptChunked(kek []byte, r io.Reader, w io.Writer, cipherSize uint64) error {
	var totalRead uint64
	chunkBuf := make([]byte, BlockSize+EncOverhead)
	var chunkIdx uint64

	for totalRead < cipherSize {
		toRead := BlockSize + EncOverhead
		remaining := cipherSize - totalRead
		if remaining < uint64(toRead) {
			toRead = remaining
		}

		n, err := io.ReadFull(r, chunkBuf[:toRead])
		if err == io.EOF && n == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read chunk: %w", err)
		}

		//#nosec:G115 // n comes from io.ReadFull and will never be negative.
		totalRead += uint64(n)

		// Verify chunk index binding
		aad := make([]byte, 8)
		binary.BigEndian.PutUint64(aad, chunkIdx)

		plainText, err := DecryptWithNonceAndAAD(kek, chunkBuf[:n], aad)
		if err != nil {
			return fmt.Errorf("failed to decrypt chunk %d: %w", chunkIdx, err)
		}

		if _, err := w.Write(plainText); err != nil {
			return fmt.Errorf("failed to write decrypted data: %w", err)
		}
		chunkIdx++
	}

	return nil
}
