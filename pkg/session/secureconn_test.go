// ABOUTME: Unit tests for SecureConn Read/Write correctness including partial reads and leftover logic.
// ABOUTME: These tests verify correctness of the readBuf→leftover refactor.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

func randomKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	return key
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

func newConnPair(t *testing.T, key []byte, suite kbc.CipherSuite) (writer, reader *SecureConn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	// Model the two roles of a real socket: the writer is the outbound endpoint,
	// the reader is the inbound endpoint, so they carry opposite nonce prefixes.
	return NewSecureConn(c1, key, suite, NoncePrefixOutbound), NewSecureConn(c2, key, suite, NoncePrefixInbound)
}

// A divergent session key must fail the AEAD open (fail closed), not silently decrypt: this proves
// a different key kills the connection rather than leaking plaintext.
func TestSecureConn_MismatchedKeyFailsClosed(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	writer := NewSecureConn(c1, randomKey(t), kbc.CipherChaCha20, NoncePrefixOutbound)
	reader := NewSecureConn(c2, randomKey(t), kbc.CipherChaCha20, NoncePrefixInbound) // divergent key

	go func() { _, _ = writer.Write(randomBytes(t, 64)) }()

	got := make([]byte, 64)
	_, err := io.ReadFull(reader, got)
	require.Error(t, err, "a divergent key must fail closed at the AEAD open, not decrypt")
}

var cipherSuites = []kbc.CipherSuite{kbc.CipherChaCha20, kbc.CipherAES256}

func TestSecureConn_RoundTrip(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)

			t.Run("small_64bytes", func(t *testing.T) {
				writer, reader := newConnPair(t, key, suite)
				original := randomBytes(t, 64)

				errCh := make(chan error, 1)
				go func() {
					_, err := writer.Write(original)
					errCh <- err
				}()

				got := make([]byte, 64)
				_, err := io.ReadFull(reader, got)
				require.NoError(t, err)
				require.Equal(t, original, got)
				require.NoError(t, <-errCh)
			})

			t.Run("large_4MiB", func(t *testing.T) {
				writer, reader := newConnPair(t, key, suite)
				const size = 4 * 1024 * 1024
				original := randomBytes(t, size)

				errCh := make(chan error, 1)
				go func() {
					_, err := writer.Write(original)
					errCh <- err
				}()

				got := make([]byte, size)
				_, err := io.ReadFull(reader, got)
				require.NoError(t, err)
				require.Equal(t, original, got)
				require.NoError(t, <-errCh)
			})
		})
	}
}

// Reads 1 MiB back in 4096-byte chunks, exercising the leftover/readBuf path.
func TestSecureConn_PartialReads(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			writer, reader := newConnPair(t, key, suite)

			const size = 1 * 1024 * 1024
			original := randomBytes(t, size)

			errCh := make(chan error, 1)
			go func() {
				_, err := writer.Write(original)
				errCh <- err
			}()

			got := make([]byte, size)
			chunk := make([]byte, 4096)
			var offset int
			for offset < size {
				remaining := size - offset
				if remaining < 4096 {
					chunk = chunk[:remaining]
				}
				_, err := io.ReadFull(reader, chunk)
				require.NoError(t, err)
				copy(got[offset:], chunk)
				offset += len(chunk)
			}

			require.Equal(t, original, got)
			require.NoError(t, <-errCh)
		})
	}
}

// SecureConn is a byte stream: messages are not framed, so 3 messages are read back with exact-size reads.
func TestSecureConn_MultiMessage(t *testing.T) {
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)

	msg1 := randomBytes(t, 100)
	msg2 := randomBytes(t, 1000)
	msg3 := randomBytes(t, 500)

	errCh := make(chan error, 1)
	go func() {
		var writeErr error
		for _, msg := range [][]byte{msg1, msg2, msg3} {
			if _, err := writer.Write(msg); err != nil {
				writeErr = err
				break
			}
		}
		errCh <- writeErr
	}()

	buf1 := make([]byte, 100)
	_, err := io.ReadFull(reader, buf1)
	require.NoError(t, err)
	require.Equal(t, msg1, buf1)

	buf2 := make([]byte, 1000)
	_, err = io.ReadFull(reader, buf2)
	require.NoError(t, err)
	require.Equal(t, msg2, buf2)

	buf3 := make([]byte, 500)
	_, err = io.ReadFull(reader, buf3)
	require.NoError(t, err)
	require.Equal(t, msg3, buf3)

	require.NoError(t, <-errCh)
}

func TestSecureConn_LargeMessage(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			writer, reader := newConnPair(t, key, suite)

			const size = 4 * 1024 * 1024
			original := randomBytes(t, size)

			errCh := make(chan error, 1)
			go func() {
				_, err := writer.Write(original)
				errCh <- err
			}()

			got := make([]byte, size)
			_, err := io.ReadFull(reader, got)
			require.NoError(t, err)
			require.Equal(t, original, got)
			require.NoError(t, <-errCh)
		})
	}
}

func TestSecureConn_ConcurrentReadWrite(t *testing.T) {
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)

	const msgCount = 100
	const msgSize = 1024
	const totalSize = msgCount * msgSize

	original := randomBytes(t, totalSize)

	errCh := make(chan error, 1)
	go func() {
		var writeErr error
		for i := 0; i < msgCount; i++ {
			chunk := original[i*msgSize : (i+1)*msgSize]
			if _, err := writer.Write(chunk); err != nil {
				writeErr = err
				break
			}
		}
		errCh <- writeErr
	}()

	got := make([]byte, totalSize)
	_, err := io.ReadFull(reader, got)
	require.NoError(t, err)
	require.Equal(t, original, got)
	require.NoError(t, <-errCh)
}

// A length header over MaxSecureMessageSize must error, not allocate an unbounded buffer.
func TestSecureReader_RejectsOversizedLength(t *testing.T) {
	key := randomKey(t)
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})

	reader := NewSecureReader(c2, key, kbc.CipherChaCha20, NoncePrefixOutbound)

	go func() {
		var header [4]byte
		// 1 byte over the limit.
		oversize := uint32(MaxSecureMessageSize + 1)
		header[0] = byte(oversize >> 24)
		header[1] = byte(oversize >> 16)
		header[2] = byte(oversize >> 8)
		header[3] = byte(oversize)
		c1.Write(header[:])
	}()

	_, err := reader.Read()
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds maximum")
}

// A message exactly at MaxSecureMessageSize is accepted (boundary of the bounds check).
func TestSecureReader_AcceptsMaxSizeLength(t *testing.T) {
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)

	original := randomBytes(t, 64)

	errCh := make(chan error, 1)
	go func() {
		_, err := writer.Write(original)
		errCh <- err
	}()

	got, err := reader.r.Read()
	require.NoError(t, err)
	require.Equal(t, original, got)
	require.NoError(t, <-errCh)
}

// Reads two messages in 5000-byte chunks, crossing message boundaries to exercise leftover logic.
func TestSecureConn_LeftoverAcrossMessages(t *testing.T) {
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)

	msg1 := randomBytes(t, 8192)
	msg2 := randomBytes(t, 4096)
	combined := append([]byte{}, msg1...)
	combined = append(combined, msg2...)
	const totalSize = 8192 + 4096

	errCh := make(chan error, 1)
	go func() {
		var writeErr error
		for _, msg := range [][]byte{msg1, msg2} {
			if _, err := writer.Write(msg); err != nil {
				writeErr = err
				break
			}
		}
		errCh <- writeErr
	}()

	got := make([]byte, totalSize)
	const chunkSize = 5000
	var offset int
	for offset < totalSize {
		remaining := totalSize - offset
		readSize := chunkSize
		if remaining < readSize {
			readSize = remaining
		}
		_, err := io.ReadFull(reader, got[offset:offset+readSize])
		require.NoError(t, err)
		offset += readSize
	}

	require.Equal(t, combined, got)
	require.NoError(t, <-errCh)
}

// QUIC control-channel keying is safe: the two ends of one conn sharing the SAME key never collide
// on key+nonce, because the dialer writes the outbound prefix and the acceptor the inbound.
func TestSecureConnRoleNoncePrefixSeparation(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20

	// Capture the first nonce each role writes under the SAME key.
	var dialerBuf, acceptorBuf bytes.Buffer
	dw := NewSecureWriterWithPrefix(&dialerBuf, key, suite, NoncePrefixOutbound)
	aw := NewSecureWriterWithPrefix(&acceptorBuf, key, suite, NoncePrefixInbound)
	_, err := dw.Write([]byte("x"))
	require.NoError(t, err)
	_, err = aw.Write([]byte("x"))
	require.NoError(t, err)

	// Frame: [4-byte length][12-byte nonce][ciphertext]. The prefix is nonce[:4].
	dPrefix := dialerBuf.Bytes()[lengthHeaderSize : lengthHeaderSize+4]
	aPrefix := acceptorBuf.Bytes()[lengthHeaderSize : lengthHeaderSize+4]
	require.NotEqual(t, dPrefix, aPrefix,
		"dialer and acceptor must write different nonce prefixes, else the shared key reuses nonces")

	// A real bidirectional round-trip over one conn with the SAME key.
	c1, c2 := net.Pipe()
	dialer := NewSecureConn(c1, key, suite, NoncePrefixOutbound)
	acceptor := NewSecureConn(c2, key, suite, NoncePrefixInbound)
	defer dialer.Close()
	defer acceptor.Close()

	go func() { _, _ = dialer.Write([]byte("ping")) }()
	got := make([]byte, 4)
	_, err = io.ReadFull(acceptor, got)
	require.NoError(t, err)
	require.Equal(t, "ping", string(got))

	go func() { _, _ = acceptor.Write([]byte("pong")) }()
	got2 := make([]byte, 4)
	_, err = io.ReadFull(dialer, got2)
	require.NoError(t, err)
	require.Equal(t, "pong", string(got2))
}
