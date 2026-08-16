// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for MasterKeySource tier selection and per-tier behaviour.
// ABOUTME: Uses the KeychainAvailable injection point to avoid hitting the real keychain.

package identity

import (
	"os"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

func TestNewMasterKeySource_KeychainTier_WhenAvailable(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("keychain not available in this environment")
	}
	dir := t.TempDir()
	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{ConfigDir: dir}))
		require.Equal(t, TierKeychain, src.Tier())
		require.Equal(t, KDFKeychain, src.KDFID())
		key := testkit.Must(src.Master())
		require.Len(t, key, 32)
		return nil
	})
}

func TestNewMasterKeySource_FileTier_Fallback(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	require.NoError(t, err)
	require.Equal(t, TierFile, src.Tier())
	require.Equal(t, KDFFile, src.KDFID())
}

func TestNewMasterKeySource_PassphraseTier_OptIn(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		PassphraseProtect: true,
		PassphraseProvider: func() (string, error) {
			return "correct horse battery staple", nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, TierPassphrase, src.Tier())
	require.Equal(t, PassphraseKDFID, src.KDFID())
}

func TestKeychainSource_StableAcrossCalls(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("keychain not available in this environment")
	}
	dir := t.TempDir()
	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{ConfigDir: dir}))
		k1 := testkit.Must(src.Master())
		k2 := testkit.Must(src.Master())
		require.Equal(t, k1, k2, "Master() returned different bytes on consecutive calls")
		return nil
	})
}

func TestFileSource_GeneratesIfMissing(t *testing.T) {
	dir := t.TempDir()
	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{
			ConfigDir:         dir,
			KeychainAvailable: func() bool { return false },
		}))
		key := testkit.Must(src.Master())
		require.Len(t, key, 32)

		// File must now exist with mode 0600.
		path := dir + "/.master.key"
		info := testkit.Must(os.Stat(path))
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		return nil
	})
}

func TestFileSource_ReadsExisting(t *testing.T) {
	dir := t.TempDir()
	knownKey := testkit.RandBytes(t, 32)
	path := dir + "/.master.key"
	require.NoError(t, os.WriteFile(path, knownKey, 0o600))

	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{
			ConfigDir:         dir,
			KeychainAvailable: func() bool { return false },
		}))
		key := testkit.Must(src.Master())
		require.Equal(t, knownKey, key, "Master() did not return the pre-existing key")
		return nil
	})
}

func TestFileSource_RejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.master.key"
	// Write 16 bytes (wrong length).
	require.NoError(t, os.WriteFile(path, make([]byte, 16), 0o600))

	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	require.NoError(t, err)

	_, err = src.Master()
	require.Error(t, err, "expected error for wrong-length key file")
}

func TestFileSource_FilePermissions0600(t *testing.T) {
	dir := t.TempDir()
	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{
			ConfigDir:         dir,
			KeychainAvailable: func() bool { return false },
		}))
		_, err := src.Master()
		require.NoError(t, err)

		info := testkit.Must(os.Stat(dir + "/.master.key"))
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		return nil
	})
}

func TestPassphraseSource_FailsOnEmptyPassphrase(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		PassphraseProtect: true,
		PassphraseProvider: func() (string, error) {
			return "", nil // empty passphrase
		},
	})
	require.NoError(t, err)

	_, err = src.Master()
	require.Error(t, err, "expected error for empty passphrase")
}

// ── GenerateMasterKey and ExternalMaster tests ──────────────────────────────

func TestGenerateMasterKey_ReturnsRandom32(t *testing.T) {
	testkit.Run(t, func() error {
		key1 := testkit.Must(GenerateMasterKey())
		require.Len(t, key1, 32)

		key2 := testkit.Must(GenerateMasterKey())

		// Two consecutive calls must return different keys (birthday bound is negligible).
		require.NotEqual(t, key1, key2, "two GenerateMasterKey calls returned the same bytes")

		// Must not be all-zero.
		require.NotEqual(t, make([]byte, 32), key1, "GenerateMasterKey returned all-zero bytes")
		return nil
	})
}

func TestNewMasterKeySource_RejectsExternalMasterAllZero(t *testing.T) {
	_, err := NewMasterKeySource(KeySourceOpts{
		ExternalMaster: make([]byte, 32), // all zeros
	})
	require.Error(t, err, "expected error for all-zero ExternalMaster")
}

func TestNewMasterKeySource_RejectsExternalMasterAllSameByte(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xAA
	}
	_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: key})
	require.Error(t, err, "expected error for all-same-byte ExternalMaster")
}

func TestNewMasterKeySource_RejectsExternalMasterSequential(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: key})
	require.Error(t, err, "expected error for sequential ExternalMaster")
}

func TestNewMasterKeySource_RejectsExternalMasterWrongLength(t *testing.T) {
	for _, size := range []int{0, 16, 64} {
		_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: make([]byte, size)})
		require.Error(t, err, "expected error for ExternalMaster of length %d", size)
	}
}

func TestNewMasterKeySource_PrefersExternalMaster(t *testing.T) {
	// A valid random-looking key (not sequential, not all-same).
	external := make([]byte, 32)
	for i := range external {
		external[i] = byte((i * 13) ^ 0x5A)
	}

	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{
			ExternalMaster:    external,
			PassphraseProtect: true, // must be ignored when ExternalMaster is set
			PassphraseProvider: func() (string, error) {
				t.Fatal("PassphraseProvider must not be called when ExternalMaster is set")
				return "", nil
			},
		}))
		require.Equal(t, TierExternal, src.Tier())

		key := testkit.Must(src.Master())
		require.Equal(t, external, key, "Master() did not return the external bytes")
		return nil
	})
}

func TestNewMasterKeySource_NilExternalMasterUsesDesktopTier(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		ExternalMaster:    nil, // explicitly nil
		KeychainAvailable: func() bool { return false },
	})
	require.NoError(t, err)
	// Should fall back to file tier.
	require.Equal(t, TierFile, src.Tier())
}
