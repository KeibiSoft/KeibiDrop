// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"bytes"
	"crypto/rand"
	"net"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/require"
)

func TestCipherNegotiation(t *testing.T) {
	t.Run("HasHardwareAES", func(t *testing.T) {
		has := crypto.HasHardwareAES()
		t.Logf("Hardware AES: %v", has)
		// On this machine (Intel i7 with AES-NI), should be true
		supported := crypto.SupportedCiphers()
		t.Logf("Supported ciphers: %v", supported)
		if has {
			require.Equal(t, crypto.CipherAES256, supported[0], "AES-256-GCM should be preferred on AES-NI hardware")
		} else {
			require.Equal(t, crypto.CipherChaCha20, supported[0], "ChaCha20 should be preferred without AES-NI")
		}
	})

	t.Run("NegotiateBothAES", func(t *testing.T) {
		local := []crypto.CipherSuite{crypto.CipherAES256, crypto.CipherChaCha20}
		remote := []crypto.CipherSuite{crypto.CipherAES256, crypto.CipherChaCha20}
		result := crypto.NegotiateCipher(local, remote)
		require.Equal(t, crypto.CipherAES256, result)
	})

	t.Run("NegotiateFallbackChaCha", func(t *testing.T) {
		local := []crypto.CipherSuite{crypto.CipherAES256, crypto.CipherChaCha20}
		remote := []crypto.CipherSuite{crypto.CipherChaCha20} // no AES support
		result := crypto.NegotiateCipher(local, remote)
		require.Equal(t, crypto.CipherChaCha20, result)
	})

	t.Run("NegotiateChaChaOnly", func(t *testing.T) {
		local := []crypto.CipherSuite{crypto.CipherChaCha20}
		remote := []crypto.CipherSuite{crypto.CipherChaCha20}
		result := crypto.NegotiateCipher(local, remote)
		require.Equal(t, crypto.CipherChaCha20, result)
	})
}

func TestSecureConnBothCiphers(t *testing.T) {
	suites := []crypto.CipherSuite{crypto.CipherChaCha20, crypto.CipherAES256}

	for _, suite := range suites {
		t.Run(string(suite), func(t *testing.T) {
			require := require.New(t)

			// Generate a random 32-byte key.
			key := make([]byte, 32)
			_, err := rand.Read(key)
			require.NoError(err)

			// Create a pipe to simulate a network connection.
			serverConn, clientConn := net.Pipe()
			defer serverConn.Close()
			defer clientConn.Close()

			server := session.NewSecureConn(serverConn, key, suite)
			client := session.NewSecureConn(clientConn, key, suite)

			// Test: client writes, server reads.
			testData := []byte("Hello from encrypted connection using " + string(suite))

			go func() {
				_, err := client.Write(testData)
				if err != nil {
					t.Errorf("client write: %v", err)
				}
			}()

			buf := make([]byte, 1024)
			n, err := server.Read(buf)
			require.NoError(err)
			require.Equal(testData, buf[:n])

			// Test: large message (1MB).
			bigData := make([]byte, 1024*1024)
			_, err = rand.Read(bigData)
			require.NoError(err)

			go func() {
				_, err := client.Write(bigData)
				if err != nil {
					t.Errorf("client write big: %v", err)
				}
			}()

			received := make([]byte, 0, len(bigData))
			for len(received) < len(bigData) {
				n, err := server.Read(buf)
				require.NoError(err)
				received = append(received, buf[:n]...)
			}
			require.True(bytes.Equal(bigData, received), "large message mismatch")
		})
	}
}

func TestKeyDerivationDomainSeparation(t *testing.T) {
	require := require.New(t)

	// Same input secrets, different cipher suites → different keys.
	secret := make([]byte, 32)
	_, err := rand.Read(secret)
	require.NoError(err)

	chachaKey, err := crypto.DeriveKey(crypto.CipherChaCha20, secret)
	require.NoError(err)

	aesKey, err := crypto.DeriveKey(crypto.CipherAES256, secret)
	require.NoError(err)

	require.False(bytes.Equal(chachaKey, aesKey), "different cipher suites must produce different keys (domain separation)")
	require.Len(chachaKey, 32)
	require.Len(aesKey, 32)
}
