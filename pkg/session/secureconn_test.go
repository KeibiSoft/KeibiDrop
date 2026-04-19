// ABOUTME: Unit tests for SecureConn Read/Write correctness including partial reads and leftover logic.
// ABOUTME: These tests verify correctness of the readBuf→leftover refactor.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
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
	return NewSecureConn(c1, key, suite), NewSecureConn(c2, key, suite)
}

var cipherSuites = []kbc.CipherSuite{kbc.CipherChaCha20, kbc.CipherAES256}

// TestSecureConn_RoundTrip verifies small and large (4 MiB) write/read round-trips.
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

// TestSecureConn_PartialReads writes 1 MiB and reads it back in 4096-byte chunks,
// exercising the leftover/readBuf path.
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

// TestSecureConn_MultiMessage writes 3 separate messages and reads them back
// with exact-size reads. SecureConn is a byte stream — messages are not framed.
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

// TestSecureConn_LargeMessage writes exactly 4 MiB and reads it back in one ReadFull.
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

// TestSecureConn_ConcurrentReadWrite writes 100 messages of 1024 bytes concurrently
// and verifies no data loss by checksumming the full byte stream.
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

// TestSecureConn_LeftoverAcrossMessages writes two messages and reads them
// in 5000-byte chunks, crossing message boundaries to exercise leftover logic.
func TestSecureConn_LeftoverAcrossMessages(t *testing.T) {
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)

	msg1 := randomBytes(t, 8192)
	msg2 := randomBytes(t, 4096)
	combined := append(msg1, msg2...)
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
