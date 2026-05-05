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
	"bytes"
	"os"
	"testing"
)

func TestNewMasterKeySource_KeychainTier_WhenAvailable(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("keychain not available in this environment")
	}
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{ConfigDir: dir})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}
	if src.Tier() != TierKeychain {
		t.Fatalf("expected tier=keychain, got %s", src.Tier())
	}
	if src.KDFID() != KDFKeychain {
		t.Fatalf("expected KDFID=%d, got %d", KDFKeychain, src.KDFID())
	}
	key, err := src.Master()
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}
}

func TestNewMasterKeySource_FileTier_Fallback(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}
	if src.Tier() != TierFile {
		t.Fatalf("expected tier=file, got %s", src.Tier())
	}
	if src.KDFID() != KDFFile {
		t.Fatalf("expected KDFID=%d, got %d", KDFFile, src.KDFID())
	}
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
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}
	if src.Tier() != TierPassphrase {
		t.Fatalf("expected tier=passphrase, got %s", src.Tier())
	}
	if src.KDFID() != PassphraseKDFID {
		t.Fatalf("expected KDFID=%d, got %d", PassphraseKDFID, src.KDFID())
	}
}

func TestKeychainSource_StableAcrossCalls(t *testing.T) {
	if !IsKeychainAvailable() {
		t.Skip("keychain not available in this environment")
	}
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{ConfigDir: dir})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	k1, err := src.Master()
	if err != nil {
		t.Fatalf("first Master(): %v", err)
	}
	k2, err := src.Master()
	if err != nil {
		t.Fatalf("second Master(): %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatal("Master() returned different bytes on consecutive calls")
	}
}

func TestFileSource_GeneratesIfMissing(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	key, err := src.Master()
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}

	// File must now exist with mode 0600.
	path := dir + "/.master.key"
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf(".master.key not created: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 0600, got %04o", info.Mode().Perm())
	}
}

func TestFileSource_ReadsExisting(t *testing.T) {
	dir := t.TempDir()
	knownKey := make([]byte, 32)
	for i := range knownKey {
		knownKey[i] = byte(i + 1)
	}
	path := dir + "/.master.key"
	if err := os.WriteFile(path, knownKey, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	key, err := src.Master()
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if !bytes.Equal(key, knownKey) {
		t.Fatal("Master() did not return the pre-existing key")
	}
}

func TestFileSource_RejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.master.key"
	// Write 16 bytes (wrong length).
	if err := os.WriteFile(path, make([]byte, 16), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	_, err = src.Master()
	if err == nil {
		t.Fatal("expected error for wrong-length key file, got nil")
	}
}

func TestFileSource_FilePermissions0600(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	_, err = src.Master()
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}

	info, err := os.Stat(dir + "/.master.key")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != os.FileMode(0o600) {
		t.Fatalf("expected mode 0600, got %04o", info.Mode().Perm())
	}
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
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	_, err = src.Master()
	if err == nil {
		t.Fatal("expected error for empty passphrase, got nil")
	}
}

// ── GenerateMasterKey and ExternalMaster tests (§L) ──────────────────────────

func TestGenerateMasterKey_ReturnsRandom32(t *testing.T) {
	key1, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey: %v", err)
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key1))
	}

	key2, err := GenerateMasterKey()
	if err != nil {
		t.Fatalf("GenerateMasterKey second call: %v", err)
	}

	// Two consecutive calls must return different keys (birthday bound is negligible).
	if string(key1) == string(key2) {
		t.Fatal("two GenerateMasterKey calls returned the same bytes")
	}

	// Must not be all-zero.
	allZero := true
	for _, b := range key1 {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("GenerateMasterKey returned all-zero bytes")
	}
}

func TestNewMasterKeySource_RejectsExternalMasterAllZero(t *testing.T) {
	_, err := NewMasterKeySource(KeySourceOpts{
		ExternalMaster: make([]byte, 32), // all zeros
	})
	if err == nil {
		t.Fatal("expected error for all-zero ExternalMaster, got nil")
	}
}

func TestNewMasterKeySource_RejectsExternalMasterAllSameByte(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0xAA
	}
	_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: key})
	if err == nil {
		t.Fatal("expected error for all-same-byte ExternalMaster, got nil")
	}
}

func TestNewMasterKeySource_RejectsExternalMasterSequential(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: key})
	if err == nil {
		t.Fatal("expected error for sequential ExternalMaster, got nil")
	}
}

func TestNewMasterKeySource_RejectsExternalMasterWrongLength(t *testing.T) {
	for _, size := range []int{0, 16, 64} {
		_, err := NewMasterKeySource(KeySourceOpts{ExternalMaster: make([]byte, size)})
		if err == nil {
			t.Fatalf("expected error for ExternalMaster of length %d, got nil", size)
		}
	}
}

func TestNewMasterKeySource_PrefersExternalMaster(t *testing.T) {
	// A valid random-looking key (not sequential, not all-same).
	external := make([]byte, 32)
	for i := range external {
		external[i] = byte((i * 13) ^ 0x5A)
	}

	src, err := NewMasterKeySource(KeySourceOpts{
		ExternalMaster:    external,
		PassphraseProtect: true, // must be ignored when ExternalMaster is set
		PassphraseProvider: func() (string, error) {
			t.Fatal("PassphraseProvider must not be called when ExternalMaster is set")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	if src.Tier() != TierExternal {
		t.Fatalf("expected TierExternal, got %s", src.Tier())
	}

	key, err := src.Master()
	if err != nil {
		t.Fatalf("Master(): %v", err)
	}
	if string(key) != string(external) {
		t.Fatal("Master() did not return the external bytes")
	}
}

func TestNewMasterKeySource_NilExternalMasterUsesDesktopTier(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		ExternalMaster:    nil, // explicitly nil
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}
	// Should fall back to file tier.
	if src.Tier() != TierFile {
		t.Fatalf("expected TierFile when ExternalMaster is nil, got %s", src.Tier())
	}
}
