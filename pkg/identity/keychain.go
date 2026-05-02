// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: OS keychain access for the identity master key (Tier 1a).
// ABOUTME: Wraps go-keyring with base64 encoding and an availability probe.

// keychain.go integrates with github.com/zalando/go-keyring to store a
// per-install 32-byte master key for at-rest identity-file encryption on
// desktop platforms. The go-keyring security model is documented at:
//
//	https://github.com/zalando/go-keyring/blob/master/SECURITY.md
//
// On macOS we use Keychain Services, on Linux libsecret over D-Bus, on
// Windows the Credential Manager (DPAPI). On systems where none of these
// are reachable IsKeychainAvailable() returns false and pkg/identity falls
// back to the file-master tier (~/.config/keibidrop/.master.key).
//
// Mobile platforms (iOS / Android) do NOT route through this file. They
// inject the master key via the ExternalMaster field in KeySourceOpts —
// see keysource.go.

package identity

import (
	"encoding/base64"
	"fmt"

	keyring "github.com/zalando/go-keyring"
)

const (
	keychainService                 = "KeibiDrop"
	keychainAccountIdentityMasterV1 = "identity-master-key-v1"
)

// keychainProbeAccount is a throwaway account used by IsKeychainAvailable to
// test the keychain roundtrip without touching real data.
const keychainProbeAccount = "identity-availability-probe"

// KeychainGet retrieves a previously stored value by account name and returns
// the raw bytes (base64-decoded). Returns an error if the account does not
// exist or the keychain is unavailable.
func KeychainGet(account string) ([]byte, error) {
	encoded, err := keyring.Get(keychainService, account)
	if err != nil {
		return nil, fmt.Errorf("keychain get %q: %w", account, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("keychain get %q: base64 decode: %w", account, err)
	}
	return decoded, nil
}

// KeychainSet stores value under account in the OS keychain. The bytes are
// base64-encoded before storage because go-keyring accepts strings only.
func KeychainSet(account string, value []byte) error {
	encoded := base64.StdEncoding.EncodeToString(value)
	if err := keyring.Set(keychainService, account, encoded); err != nil {
		return fmt.Errorf("keychain set %q: %w", account, err)
	}
	return nil
}

// KeychainDelete removes the entry for account from the OS keychain.
func KeychainDelete(account string) error {
	if err := keyring.Delete(keychainService, account); err != nil {
		return fmt.Errorf("keychain delete %q: %w", account, err)
	}
	return nil
}

// IsKeychainAvailable probes whether the OS keychain is functional by
// performing a set/get/delete roundtrip with a temporary test value.
// Returns false on any error (e.g. no D-Bus session, unsupported platform).
func IsKeychainAvailable() bool {
	probe := []byte("probe")
	if err := KeychainSet(keychainProbeAccount, probe); err != nil {
		return false
	}
	_, err := KeychainGet(keychainProbeAccount)
	_ = KeychainDelete(keychainProbeAccount)
	return err == nil
}
