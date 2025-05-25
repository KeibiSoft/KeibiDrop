package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

const KeySize = chacha20poly1305.KeySize
const EncOverhead = uint64(chacha20poly1305.NonceSize + chacha20poly1305.Overhead)
const BlockSize = uint64(2 << 16) // On linux cp works with blocks of 128KiB, we use double.

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
		if err == io.EOF && n == 0 {
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
