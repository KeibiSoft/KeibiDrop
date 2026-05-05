// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Integration tests for the OS keychain wrappers.
// ABOUTME: Tests are skipped automatically on systems without a functioning keychain.

package identity

import (
	"crypto/rand"
	"testing"

	"github.com/stretchr/testify/require"
	keyring "github.com/zalando/go-keyring"
)

// TestIsKeychainAvailable_True verifies the availability probe on dev boxes
// where libsecret / GNOME keyring is running. On CI without a keychain the
// test is skipped by design.
func TestIsKeychainAvailable_True(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("no keychain available — skipping")
	}
	// If IsKeychainAvailable returned true we've already done a roundtrip.
	// No additional assertion needed; reaching here means the keychain works.
}

func TestKeychainSetGetDelete_Roundtrip(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("no keychain available — skipping")
	}
	req := require.New(t)

	account := "identity-test-roundtrip"

	// Make sure we start clean.
	_ = KeychainDelete(account)

	value := make([]byte, 32)
	_, err := rand.Read(value)
	req.NoError(err)

	req.NoError(KeychainSet(account, value))

	got, err := KeychainGet(account)
	req.NoError(err)
	req.Equal(value, got)

	req.NoError(KeychainDelete(account))

	// After deletion Get must return an error.
	_, err = KeychainGet(account)
	req.Error(err, "Get after Delete must return an error")
}

// TestKeychainGet_Missing_ReturnsError ensures that requesting a non-existent
// account surfaces an error rather than returning empty bytes silently.
func TestKeychainGet_Missing_ReturnsError(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("no keychain available — skipping")
	}
	req := require.New(t)

	account := "identity-test-nonexistent-key-" + randomHex(t, 8)
	// Ensure it truly doesn't exist.
	_ = keyring.Delete(keychainService, account)

	_, err := KeychainGet(account)
	req.Error(err, "Get on missing account must return non-nil error")
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	const hexChars = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexChars[v>>4]
		out[i*2+1] = hexChars[v&0xf]
	}
	return string(out)
}
