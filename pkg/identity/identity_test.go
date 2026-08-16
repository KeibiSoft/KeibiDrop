// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// newTestMasterKeySource returns a deterministic in-memory MasterKeySource
// suitable for unit tests. It always returns the same 32-byte key so tests
// are reproducible without touching the OS keychain.
func newTestMasterKeySource(_ *testing.T) MasterKeySource {
	return &stubMasterKeySource{
		key: []byte("test-master-key-32-bytes-exactly"),
	}
}

type stubMasterKeySource struct {
	key []byte
}

func (s *stubMasterKeySource) Master() ([]byte, error) { return s.key, nil }
func (s *stubMasterKeySource) Tier() Tier              { return TierFile }
func (s *stubMasterKeySource) KDFID() uint8            { return KDFFile }

// ── Existing tests (updated to pass MasterKeySource) ────────────────────────

func TestCreateAndLoad(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		id := testkit.Must(LoadOrCreate(dir, src))
		require.NotEqual(t, "", id.Fingerprint, "empty fingerprint")
		require.NotNil(t, id.Keys)
		require.NoError(t, id.Keys.Validate())

		// Verify encrypted file exists.
		path := filepath.Join(dir, identityFile)
		info := testkit.Must(os.Stat(path))
		require.NotEqual(t, int64(0), info.Size(), "identity file is empty")
		return nil
	})
}

func TestFingerprintStability(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		id1 := testkit.Must(LoadOrCreate(dir, src))
		id2 := testkit.Must(LoadOrCreate(dir, src))
		require.Equal(t, id1.Fingerprint, id2.Fingerprint, "fingerprint changed across loads")
		return nil
	})
}

func TestLoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := Load(dir, src)
	require.True(t, os.IsNotExist(err), "expected os.IsNotExist, got: %v", err)
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		id := testkit.Must(LoadOrCreate(dir, src))

		// Load explicitly.
		loaded := testkit.Must(Load(dir, src))
		require.Equal(t, id.Fingerprint, loaded.Fingerprint)

		// Verify key round-trip: the loaded keys produce the same fingerprint.
		fp := testkit.Must(loaded.Keys.Fingerprint())
		require.Equal(t, id.Fingerprint, fp)
		return nil
	})
}

func TestEncryptedFileNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dir, src)
	require.NoError(t, err)

	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	require.NoError(t, err)

	// Encrypted data should not contain JSON markers.
	for _, marker := range []string{`"x25519_seed"`, `"mlkem_seed"`, `"created_at"`} {
		require.False(t, contains(data, []byte(marker)), "identity file contains plaintext marker %q", marker)
	}
}

func contains(data, sub []byte) bool {
	for i := 0; i <= len(data)-len(sub); i++ {
		match := true
		for j := range sub {
			if data[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func TestKeyFunctionality(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	id, err := LoadOrCreate(dir, src)
	require.NoError(t, err)

	// Verify ML-KEM encapsulate/decapsulate round-trip.
	ss, ct := id.Keys.MlKemPublic.Encapsulate()
	ss2, err := id.Keys.MlKemPrivate.Decapsulate(ct)
	require.NoError(t, err)
	require.Equal(t, ss, ss2, "shared secret mismatch")

	// Verify X25519 ECDH.
	_, err = id.Keys.X25519Private.ECDH(id.Keys.X25519Public)
	require.NoError(t, err)
}

// ── Envelope / round-trip tests ───────────────────────────────────────────────

func TestRoundTrip_KeychainTierStub(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		id := testkit.Must(LoadOrCreate(dir, src))
		loaded := testkit.Must(Load(dir, src))
		require.Equal(t, id.Fingerprint, loaded.Fingerprint)
		return nil
	})
}

func TestRoundTrip_FileTier(t *testing.T) {
	dir := t.TempDir()

	testkit.Run(t, func() error {
		src := testkit.Must(NewMasterKeySource(KeySourceOpts{
			ConfigDir:         dir,
			KeychainAvailable: func() bool { return false },
		}))
		id := testkit.Must(LoadOrCreate(dir, src))
		loaded := testkit.Must(Load(dir, src))
		require.Equal(t, id.Fingerprint, loaded.Fingerprint)
		return nil
	})
}

func TestEnvelopeAADBinding(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dir, src)
	require.NoError(t, err)

	path := filepath.Join(dir, identityFile)
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Flip a byte in the salt (bytes 8..23 of the 24-byte header).
	data[10] ^= 0xFF
	require.NoError(t, os.WriteFile(path, data, 0o600))

	_, err = Load(dir, src)
	require.Error(t, err, "expected error after tampering header")
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	// Ensure dir has 0750.
	require.NoError(t, os.Chmod(dir, 0o750))

	src := newTestMasterKeySource(t)
	_, err := LoadOrCreate(dir, src)
	require.NoError(t, err)

	path := filepath.Join(dir, identityFile)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestSchemaVersionForwardCompat(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		// Craft a serializedIdentity with a schema_version far in the future.
		id := testkit.Must(create())

		s := serializedIdentity{
			SchemaVersion: 99,
			X25519Seed:    id.Keys.X25519Private.Bytes(),
			MLKEMSeed:     id.Keys.MlKemPrivate.Bytes(),
			CreatedAt:     id.CreatedAt,
		}
		jsonBytes := testkit.Must(json.Marshal(&s))

		// Write it as an envelope.
		salt := testkit.Must(kbc.RandomBytes(envelopeSaltSize))
		header := EnvelopeHeader{KDFID: src.KDFID()}
		copy(header.Salt[:], salt)

		perFileKey := testkit.Must(derivePerFileKey(src, header, "keibidrop-identity-file-v1"))
		blob := testkit.Must(kbc.EncryptWithAAD(perFileKey, jsonBytes, header.AAD()))
		copy(header.Nonce[:], blob[:kbc.NonceSize])
		ctAndTag := blob[kbc.NonceSize:]

		path := filepath.Join(dir, identityFile)
		require.NoError(t, WriteFileAtomic(path, MarshalEnvelope(header, ctAndTag), 0o600))

		_, err := Load(dir, src)
		require.ErrorIs(t, err, ErrIdentityNewerSchema)
		return nil
	})
}

func TestKeySubstitution_DifferentMasterKeyFails(t *testing.T) {
	dirA := t.TempDir()
	srcA := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dirA, srcA)
	require.NoError(t, err)

	// Different master key — derive a different source.
	srcB := &stubMasterKeySource{
		key: []byte("other-master-key-32-bytes-exactly"),
	}

	_, err = Load(dirA, srcB)
	require.Error(t, err, "expected error when loading with wrong master key")

	// Original file must remain in place (no auto-rename).
	_, statErr := os.Stat(filepath.Join(dirA, identityFile))
	require.False(t, os.IsNotExist(statErr), "original identity.enc must remain in place (no auto-rename)")
}
