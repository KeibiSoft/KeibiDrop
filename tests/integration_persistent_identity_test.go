// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Integration tests for the persistent identity path.
// ABOUTME: Exercises LoadOrCreate, passphrase tier, corruption, and vulnerability guards.

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/KeibiSoft/KeibiDrop/pkg/identity"
	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// newFileSource returns a file-tier MasterKeySource that never uses the
// OS keychain, making tests self-contained.
func newFileSource(t *testing.T, configDir string) identity.MasterKeySource {
	t.Helper()
	src, err := identity.NewMasterKeySource(identity.KeySourceOpts{
		ConfigDir:         configDir,
		KeychainAvailable: func() bool { return false },
	})
	require.NoError(t, err)
	return src
}

// newPassphraseSource returns a passphrase-tier MasterKeySource with the
// given passphrase string.
func newPassphraseSource(t *testing.T, passphrase string) identity.MasterKeySource {
	t.Helper()
	src, err := identity.NewMasterKeySource(identity.KeySourceOpts{
		PassphraseProtect: true,
		PassphraseProvider: func() (string, error) {
			return passphrase, nil
		},
	})
	require.NoError(t, err)
	return src
}

// identityFilePath returns the expected identity.enc path inside configDir.
const identityFileName = "identity.enc"

// ── tests ─────────────────────────────────────────────────────────────────────

// TestPersistentIdentitySurvivesRestart saves an identity, drops state, reloads
// with a fresh source, and verifies the fingerprint is byte-identical.
func TestPersistentIdentitySurvivesRestart(t *testing.T) {
	tmp := t.TempDir()
	src1 := newFileSource(t, tmp)

	id1, err := identity.LoadOrCreate(tmp, src1)
	require.NoError(t, err)
	require.NotEmpty(t, id1.Fingerprint)

	// Drop in-memory state entirely; build a second independent source.
	src2 := newFileSource(t, tmp)
	id2, err := identity.LoadOrCreate(tmp, src2)
	require.NoError(t, err)

	require.Equal(t, id1.Fingerprint, id2.Fingerprint,
		"fingerprint must survive a simulated restart")

	// On-disk file must have the KDID magic.
	raw, err := os.ReadFile(filepath.Join(tmp, identityFileName))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(raw, []byte("KDID")),
		"identity.enc must start with KDID magic")
}

// TestPassphraseTier_RoundTrip verifies save/load with a passphrase key source,
// and that a wrong passphrase triggers an error.
func TestPassphraseTier_RoundTrip(t *testing.T) {
	const goodPassphrase = "test-passphrase-with-spaces and unicode é 🔐"
	const badPassphrase = "completely-wrong-passphrase"

	tmp := t.TempDir()

	// Save with the good passphrase.
	src1 := newPassphraseSource(t, goodPassphrase)
	id1, err := identity.LoadOrCreate(tmp, src1)
	require.NoError(t, err)
	require.NotEmpty(t, id1.Fingerprint)

	// Reload with the same passphrase — fingerprint must match.
	src2 := newPassphraseSource(t, goodPassphrase)
	id2, err := identity.LoadOrCreate(tmp, src2)
	require.NoError(t, err)
	require.Equal(t, id1.Fingerprint, id2.Fingerprint,
		"fingerprint must survive passphrase-tier round-trip")

	// Write the good-passphrase identity so we can test the wrong-key error.
	require.NoError(t, id1.Save(tmp, src2))

	srcBad2 := newPassphraseSource(t, badPassphrase)
	_, err = identity.Load(tmp, srcBad2)
	require.Error(t, err)
	
	require.Error(t, err,
		"wrong passphrase must produce *error, got: %T %v", err, err)

	// Original file must remain in place (no auto-rename).
	_, statErr := os.Stat(filepath.Join(tmp, identityFileName))
	require.NoError(t, statErr, "original identity.enc must remain in place")
}

// TestCorruptionReturnsTypedError writes a valid identity, corrupts the
// AEAD tag, and verifies that Load returns error without
// renaming the file.
func TestCorruptionReturnsTypedError(t *testing.T) {
	tmp := t.TempDir()
	src := newFileSource(t, tmp)

	_, err := identity.LoadOrCreate(tmp, src)
	require.NoError(t, err)

	path := filepath.Join(tmp, identityFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, len(data) >= 8, "identity file must be at least 8 bytes")

	// Corrupt the last 8 bytes (breaks the AEAD tag).
	corrupted := make([]byte, len(data))
	copy(corrupted, data)
	for i := len(corrupted) - 8; i < len(corrupted); i++ {
		corrupted[i] ^= 0xFF
	}
	require.NoError(t, os.WriteFile(path, corrupted, 0o600))

	// Load must fail with error.
	srcFresh := newFileSource(t, tmp)
	_, err = identity.Load(tmp, srcFresh)
	require.Error(t, err)
	
	require.Error(t, err,
		"corrupted file must yield *error, got: %T %v", err, err)

	// The original file must remain (no auto-rename).
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "original identity.enc must remain in place (no auto-rename)")
}

// TestEnablePersistentIdentity_FullStack exercises the EnablePersistentIdentity
// path end-to-end through the KeibiDrop type.
func TestEnablePersistentIdentity_FullStack(t *testing.T) {
	tmp := t.TempDir()

	logger := slog.New(slog.NewTextHandler(os.Stdout,
		&slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("http://127.0.0.1:54321")

	newKD := func() *common.KeibiDrop {
		t.Helper()
		inPort := getFreePortInRange(t, 26700, 26799)
		outPort := getFreePortInRange(t, 26800, 26899)
		kd, err := common.NewKeibiDropWithIP(
			context.Background(), logger,
			false,
			relayURL,
			inPort, outPort,
			t.TempDir(),
			t.TempDir(),
			false, false,
			"::1",
		)
		require.NoError(t, err)
		t.Cleanup(kd.Shutdown)
		return kd
	}

	// Round 1: EnablePersistentIdentity on a fresh configDir.
	kd1 := newKD()
	err := kd1.EnablePersistentIdentity(tmp, common.EnableOpts{})
	require.NoError(t, err)
	require.NotNil(t, kd1.Identity)
	require.NotNil(t, kd1.AddressBook)
	fp1 := kd1.Identity.Fingerprint
	require.NotEmpty(t, fp1)

	// On-disk file must have KDID magic.
	raw, err := os.ReadFile(filepath.Join(tmp, identityFileName))
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(raw, []byte("KDID")),
		"identity.enc must start with KDID magic")

	// Round 2: a fresh KeibiDrop instance using the same configDir must
	// produce the same fingerprint.
	kd2 := newKD()
	err = kd2.EnablePersistentIdentity(tmp, common.EnableOpts{})
	require.NoError(t, err)
	require.Equal(t, fp1, kd2.Identity.Fingerprint,
		"fingerprint must be stable across restarts via EnablePersistentIdentity")

	// Round 3: corrupt the file, attempt load → typed error.
	path := filepath.Join(tmp, identityFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	corruptData := make([]byte, len(data))
	copy(corruptData, data)
	for i := len(corruptData) - 8; i < len(corruptData); i++ {
		corruptData[i] ^= 0xFF
	}
	require.NoError(t, os.WriteFile(path, corruptData, 0o600))

	kd3 := newKD()
	err = kd3.EnablePersistentIdentity(tmp, common.EnableOpts{})
	require.Error(t, err)
	
	require.Error(t, err,
		"corrupted file must yield *error")

	// Original file must remain in place (no auto-rename).
	_, statErr := os.Stat(path)
	require.NoError(t, statErr, "original file must remain in place")
}

// ── vulnerability tests ───────────────────────────────────────────────────────

// TestVuln_NonEnvelopeFileRejected writes random bytes to identity.enc and
// verifies that LoadOrCreate returns *error (not a silent
// success or a panic).
func TestVuln_NonEnvelopeFileRejected(t *testing.T) {
	tmp := t.TempDir()
	src := newFileSource(t, tmp)

	// Write random bytes that look nothing like a KDID envelope.
	randomBytes := make([]byte, 128)
	for i := range randomBytes {
		randomBytes[i] = byte(i ^ 0xA5)
	}
	path := filepath.Join(tmp, identityFileName)
	require.NoError(t, os.WriteFile(path, randomBytes, 0o600))

	_, err := identity.Load(tmp, src)
	require.Error(t, err, "non-envelope file must be rejected")
	
	require.Error(t, err,
		"non-envelope file must produce *error, got: %T %v", err, err)
}

// TestVuln_KeySubstitution_DifferentMasterKeyFails saves with master key A,
// then overwrites .master.key with a different 32-byte key B and verifies
// that loading produces error.
func TestVuln_KeySubstitution_DifferentMasterKeyFails(t *testing.T) {
	tmp := t.TempDir()
	src := newFileSource(t, tmp)

	_, err := identity.LoadOrCreate(tmp, src)
	require.NoError(t, err)

	// Build a deterministic substitute key that isn't all-same or sequential.
	keyB := make([]byte, 32)
	for i := range keyB {
		keyB[i] = byte((i*7 + 13) & 0xFF)
	}
	masterKeyPath := filepath.Join(tmp, ".master.key")
	require.NoError(t, os.WriteFile(masterKeyPath, keyB, 0o600))

	// Build a new source backed by the substituted key.
	srcB := newFileSource(t, tmp)
	_, err = identity.Load(tmp, srcB)
	require.Error(t, err)
	
	require.Error(t, err,
		"key substitution must produce *error, got: %T %v", err, err)
}

// TestVuln_AADTampering_HeaderFlip flips a bit in the salt portion of the
// envelope header (bytes 8–23) and verifies that loading fails with a typed
// error due to AEAD tag mismatch.
func TestVuln_AADTampering_HeaderFlip(t *testing.T) {
	tmp := t.TempDir()
	src := newFileSource(t, tmp)

	_, err := identity.LoadOrCreate(tmp, src)
	require.NoError(t, err)

	path := filepath.Join(tmp, identityFileName)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, len(data) >= 24, "envelope must be at least 24 bytes")

	// Flip a bit in the salt (byte offset 10, within the 8..23 range).
	data[10] ^= 0x01
	require.NoError(t, os.WriteFile(path, data, 0o600))

	srcFresh := newFileSource(t, tmp)
	_, err = identity.Load(tmp, srcFresh)
	require.Error(t, err, "tampered AAD must cause load failure")
	
	require.Error(t, err,
		"AAD tampering must produce *error, got: %T %v", err, err)
}

// TestVuln_SchemaVersionForwardCompat manually re-encrypts the identity with
// schema_version=99 and verifies that Load returns ErrIdentityNewerSchema.
func TestVuln_SchemaVersionForwardCompat(t *testing.T) {
	tmp := t.TempDir()
	src := newFileSource(t, tmp)

	id, err := identity.LoadOrCreate(tmp, src)
	require.NoError(t, err)

	path := filepath.Join(tmp, identityFileName)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	// Parse the envelope to get header and key material.
	header, ctAndTag, err := identity.ParseEnvelope(raw)
	require.NoError(t, err)

	// Reconstruct the plaintext: derive per-file key, decrypt.
	masterKey, err := src.Master()
	require.NoError(t, err)

	perFileKey, err := kbc.DeriveFileEncryptionKey(masterKey, header.Salt[:], "keibidrop-identity-file-v1")
	require.NoError(t, err)

	blob := make([]byte, kbc.NonceSize+len(ctAndTag))
	copy(blob[:kbc.NonceSize], header.Nonce[:])
	copy(blob[kbc.NonceSize:], ctAndTag)

	plaintext, err := kbc.DecryptWithAAD(perFileKey, blob, header.AAD())
	require.NoError(t, err)

	// Unmarshal, bump schema_version, re-marshal.
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(plaintext, &payload))
	payload["schema_version"] = 99
	mutated, err := json.Marshal(payload)
	require.NoError(t, err)

	// Re-encrypt using the SAME per-file key derivation with a new salt.
	salt, err := kbc.RandomBytes(16)
	require.NoError(t, err)
	var newHeader identity.EnvelopeHeader
	newHeader.KDFID = src.KDFID()
	copy(newHeader.Salt[:], salt)

	newPerFileKey, err := kbc.DeriveFileEncryptionKey(masterKey, newHeader.Salt[:], "keibidrop-identity-file-v1")
	require.NoError(t, err)

	newBlob, err := kbc.EncryptWithAAD(newPerFileKey, mutated, newHeader.AAD())
	require.NoError(t, err)

	copy(newHeader.Nonce[:], newBlob[:kbc.NonceSize])
	ctAndTagNew := newBlob[kbc.NonceSize:]

	require.NoError(t, os.WriteFile(
		path,
		identity.MarshalEnvelope(newHeader, ctAndTagNew),
		0o600,
	))

	// Load must return ErrIdentityNewerSchema.
	srcFresh := newFileSource(t, tmp)
	_, err = identity.Load(tmp, srcFresh)
	require.Error(t, err)
	require.True(t, errors.Is(err, identity.ErrIdentityNewerSchema),
		"schema_version=99 must produce ErrIdentityNewerSchema, got: %v", err)

	// Silence unused variable.
	_ = id.Fingerprint
}

// TestContacts_RoundTrip verifies the address book can be saved and reloaded
// with fingerprints and names intact.
func TestContacts_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	src1 := newFileSource(t, tmp)

	ab1, err := identity.LoadAddressBook(tmp, src1)
	require.NoError(t, err)

	require.NoError(t, ab1.Add("Alice", "fingerprint-aaa"))
	require.NoError(t, ab1.Add("Bob", "fingerprint-bbb"))
	require.NoError(t, ab1.Save())

	// Fresh source, fresh load.
	src2 := newFileSource(t, tmp)
	ab2, err := identity.LoadAddressBook(tmp, src2)
	require.NoError(t, err)

	require.Equal(t, 2, ab2.Count(), "address book must contain 2 contacts after reload")

	alice := ab2.Lookup("fingerprint-aaa")
	require.NotNil(t, alice, "Alice must be in the reloaded address book")
	require.Equal(t, "Alice", alice.Name)

	bob := ab2.Lookup("fingerprint-bbb")
	require.NotNil(t, bob, "Bob must be in the reloaded address book")
	require.Equal(t, "Bob", bob.Name)
}
