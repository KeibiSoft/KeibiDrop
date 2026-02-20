// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"

	"log/slog"

	"github.com/stretchr/testify/require"
)

// TestRelay_RegisterAndFetch tests the full relay round-trip:
// register encrypted blob, fetch it, decrypt it, verify contents match.
func TestRelay_RegisterAndFetch(t *testing.T) {
	require := require.New(t)
	relay := NewMockRelay()
	defer relay.Close()

	// Generate a session to get a valid fingerprint
	logger := slog.Default()
	sess, err := session.InitSession(logger, 26432, 26431)
	require.NoError(err)

	fp := sess.OwnFingerprint

	// Derive relay keys from fingerprint
	roomPassword, err := crypto.ExtractRoomPassword(fp)
	require.NoError(err)

	lookupKey, encryptionKey, err := crypto.DeriveRelayKeys(roomPassword)
	require.NoError(err)

	lookupToken := base64.RawURLEncoding.EncodeToString(lookupKey)

	// Encrypt a test payload
	plaintext := []byte(`{"fingerprint":"test","listen":{"ip":"::1","port":26431}}`)
	encryptedBlob, err := crypto.Encrypt(encryptionKey, plaintext)
	require.NoError(err)

	blobB64 := base64.RawURLEncoding.EncodeToString(encryptedBlob)

	// Register
	body := `{"blob":"` + blobB64 + `"}`
	req, err := http.NewRequest("POST", relay.URL()+"/register", strings.NewReader(body))
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+lookupToken)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	require.Equal(1, relay.EntryCount())

	// Fetch
	req, err = http.NewRequest("GET", relay.URL()+"/fetch", nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer "+lookupToken)

	resp, err = http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(err)

	// Decode and decrypt
	var fetched struct {
		Blob string `json:"blob"`
	}
	require.NoError(json.Unmarshal(respBody, &fetched))

	encBytes, err := base64.RawURLEncoding.DecodeString(fetched.Blob)
	require.NoError(err)

	decrypted, err := crypto.Decrypt(encryptionKey, encBytes)
	require.NoError(err)

	require.Equal(plaintext, decrypted)
}

// TestRelay_FetchNotFound tests that fetching before registration returns 404.
func TestRelay_FetchNotFound(t *testing.T) {
	require := require.New(t)
	relay := NewMockRelay()
	defer relay.Close()

	req, err := http.NewRequest("GET", relay.URL()+"/fetch", nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer nonexistent-token")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestRelay_TokenIsolation tests that different tokens have separate storage.
func TestRelay_TokenIsolation(t *testing.T) {
	require := require.New(t)
	relay := NewMockRelay()
	defer relay.Close()

	// Register with token A
	body := `{"blob":"data-for-token-A"}`
	req, err := http.NewRequest("POST", relay.URL()+"/register", strings.NewReader(body))
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-A")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// Register with token B
	body = `{"blob":"data-for-token-B"}`
	req, err = http.NewRequest("POST", relay.URL()+"/register", strings.NewReader(body))
	require.NoError(err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer token-B")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	require.Equal(2, relay.EntryCount())

	// Fetch with token A — should get A's data
	req, err = http.NewRequest("GET", relay.URL()+"/fetch", nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer token-A")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode)

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(err)

	var fetched struct {
		Blob string `json:"blob"`
	}
	require.NoError(json.Unmarshal(respBody, &fetched))
	require.Equal("data-for-token-A", fetched.Blob)

	// Fetch with token B — should get B's data
	req, err = http.NewRequest("GET", relay.URL()+"/fetch", nil)
	require.NoError(err)
	req.Header.Set("Authorization", "Bearer token-B")

	resp, err = http.DefaultClient.Do(req)
	require.NoError(err)
	require.Equal(http.StatusOK, resp.StatusCode)

	respBody, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(err)

	require.NoError(json.Unmarshal(respBody, &fetched))
	require.Equal("data-for-token-B", fetched.Blob)
}
