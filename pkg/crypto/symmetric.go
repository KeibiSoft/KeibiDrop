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
const BlockSize = uint64(2 << 16) // linux cp uses 128KiB blocks; we use double.

// nonceCounterBits is the width of the per-epoch message counter within the nonce. The
// packed state holds the 16-bit epoch in the high bits, the 48-bit counter in the low bits.
const nonceCounterBits = 48

// nonceCounterMask selects the 48-bit counter out of the packed state.
const nonceCounterMask = (uint64(1) << nonceCounterBits) - 1

// NonceGenerator provides deterministic per-direction AEAD nonces of the form
// [4-byte prefix][2-byte big-endian epoch][6-byte big-endian counter] = 12 bytes. The prefix
// distinguishes directions. The epoch (key generation) is bumped by the ratchet; the 48-bit
// counter resets to 0 on each bump. Epoch and counter share one atomic word so every Next
// reads a consistent pair; the caller serializes SetEpoch against Next. At epoch 0 the layout
// is byte-identical to the old [4-byte prefix][8-byte counter] format, so a ratchet-unaware
// peer stays interoperable until the first bump.
type NonceGenerator struct {
	prefix [4]byte       // Direction/session identifier.
	state  atomic.Uint64 // (epoch << 48) | counter.
}

// NewNonceGenerator creates a nonce generator with the given prefix.
// Use different prefixes for inbound vs outbound to avoid nonce reuse.
func NewNonceGenerator(prefix uint32) *NonceGenerator {
	ng := &NonceGenerator{}
	binary.BigEndian.PutUint32(ng.prefix[:], prefix)
	return ng
}

// ErrNonceOverflow is returned by Next when the 48-bit per-epoch counter would wrap into the
// epoch bytes and reuse a (key, nonce) pair. The ratchet rotates far below 2^48 messages, so
// reaching this means rotation stalled; the caller closes that one connection.
var ErrNonceOverflow = errors.New("crypto: nonce counter overflow within an epoch (missing rekey)")

// Next returns the next nonce and advances the counter. Thread-safe. Returns ErrNonceOverflow
// if the 48-bit counter would wrap within an epoch, since a wrap carries into the epoch bytes
// and reuses a (key, nonce) pair.
func (ng *NonceGenerator) Next() ([NonceSize]byte, error) {
	// state packs epoch|counter; its big-endian uint64 is exactly [2B epoch][6B counter],
	// so one PutUint64 emits both halves.
	s := ng.state.Add(1)
	if s&nonceCounterMask == 0 {
		return [NonceSize]byte{}, ErrNonceOverflow
	}
	var nonce [NonceSize]byte
	copy(nonce[:4], ng.prefix[:])
	binary.BigEndian.PutUint64(nonce[4:], s)
	return nonce, nil
}

// SetEpoch installs a new key epoch and resets the counter, so the next Next emits
// counter 1 under that epoch. The caller must serialize SetEpoch against Next.
func (ng *NonceGenerator) SetEpoch(epoch uint16) {
	ng.state.Store(uint64(epoch) << nonceCounterBits)
}

// Count returns the current per-epoch counter (for monitoring/debugging).
func (ng *NonceGenerator) Count() uint64 {
	return ng.state.Load() & nonceCounterMask
}

// Epoch returns the current key epoch, the high 16 bits of the state (for monitoring).
func (ng *NonceGenerator) Epoch() uint16 {
	return uint16(ng.state.Load() >> nonceCounterBits)
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

// EncryptWithAAD encrypts plainText using KEK with ChaCha20-Poly1305 and authenticated
// associated data (aad is authenticated but not included in the ciphertext; nil for none).
// Returns [nonce | ciphertext+MAC], or error.
func EncryptWithAAD(kek, plainText, aad []byte) ([]byte, error) {
	if len(kek) != KeySize {
		return nil, errors.New("invalid key size")
	}

	aead, err := chacha20poly1305.New(kek)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}

	// Nonce is freshly generated above, not hardcoded.
	cipherText := aead.Seal(nil, nonce, plainText, aad) // #nosec G407

	result := make([]byte, len(nonce)+len(cipherText))
	copy(result, nonce)
	copy(result[NonceSize:], cipherText)

	return result, nil
}

// DecryptWithAAD decrypts [nonce | ciphertext+MAC] using KEK, verifying the associated
// data (aad). The aad must match what EncryptWithAAD used, or authentication fails; nil
// when no AAD was used. Returns plainText or error if authentication fails.
func DecryptWithAAD(kek, input, aad []byte) ([]byte, error) {
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

// Encrypt encrypts plainText using KEK with ChaCha20-Poly1305.
// Returns [nonce | ciphertext+MAC], or error.
func Encrypt(kek, plainText []byte) ([]byte, error) {
	return EncryptWithAAD(kek, plainText, nil)
}

// Decrypt decrypts [nonce | ciphertext+MAC] using KEK.
// Returns plainText or error if authentication fails.
func Decrypt(kek, input []byte) ([]byte, error) {
	return DecryptWithAAD(kek, input, nil)
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

		encryptedChunk, err := Encrypt(kek, buf[:n])
		if err != nil {
			return err
		}

		if _, err := w.Write(encryptedChunk); err != nil {
			return err
		}
	}

	return nil
}

func DecryptChunked(kek []byte, r io.Reader, w io.Writer, cipherSize uint64) error {
	var totalRead uint64
	chunkBuf := make([]byte, BlockSize+EncOverhead)

	for totalRead < cipherSize {
		toRead := BlockSize + EncOverhead
		remaining := cipherSize - totalRead
		if remaining < uint64(toRead) {
			toRead = remaining
		}

		n, err := io.ReadFull(r, chunkBuf[:toRead])
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to read chunk: %w", err)
		}

		//#nosec:G115 // n comes from io.ReadFull and will never be negative.
		totalRead += uint64(n)

		plainText, err := Decrypt(kek, chunkBuf[:n])
		if err != nil {
			return fmt.Errorf("failed to decrypt chunk: %w", err)
		}

		if _, err := w.Write(plainText); err != nil {
			return fmt.Errorf("failed to write decrypted data: %w", err)
		}
	}

	return nil
}
