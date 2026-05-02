// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
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

	id, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if id.Fingerprint == "" {
		t.Fatal("empty fingerprint")
	}
	if id.Keys == nil {
		t.Fatal("nil keys")
	}
	if err := id.Keys.Validate(); err != nil {
		t.Fatalf("invalid keys: %v", err)
	}

	// Verify encrypted file exists.
	path := filepath.Join(dir, identityFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("identity file not found: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("identity file is empty")
	}
}

func TestFingerprintStability(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	id1, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}

	id2, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}

	if id1.Fingerprint != id2.Fingerprint {
		t.Fatalf("fingerprint changed across loads: %s vs %s",
			id1.Fingerprint, id2.Fingerprint)
	}
}

func TestLoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := Load(dir, src)
	if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got: %v", err)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	id, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Load explicitly.
	loaded, err := Load(dir, src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: %s vs %s", loaded.Fingerprint, id.Fingerprint)
	}

	// Verify key round-trip: the loaded keys produce the same fingerprint.
	fp, err := loaded.Keys.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint from loaded keys: %v", err)
	}
	if fp != id.Fingerprint {
		t.Fatalf("computed fingerprint mismatch: %s vs %s", fp, id.Fingerprint)
	}
}

func TestEncryptedFileNotPlaintext(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, identityFile))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	// Encrypted data should not contain JSON markers.
	for _, marker := range []string{`"x25519_seed"`, `"mlkem_seed"`, `"created_at"`} {
		if contains(data, []byte(marker)) {
			t.Fatalf("identity file contains plaintext marker %q", marker)
		}
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
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	// Verify ML-KEM encapsulate/decapsulate round-trip.
	ss, ct := id.Keys.MlKemPublic.Encapsulate()
	ss2, err := id.Keys.MlKemPrivate.Decapsulate(ct)
	if err != nil {
		t.Fatalf("mlkem decapsulate: %v", err)
	}
	if len(ss) != len(ss2) {
		t.Fatal("shared secret length mismatch")
	}
	for i := range ss {
		if ss[i] != ss2[i] {
			t.Fatal("shared secret mismatch")
		}
	}

	// Verify X25519 ECDH.
	_, err = id.Keys.X25519Private.ECDH(id.Keys.X25519Public)
	if err != nil {
		t.Fatalf("x25519 ECDH: %v", err)
	}
}

// ── Envelope / round-trip tests ───────────────────────────────────────────────

func TestRoundTrip_KeychainTierStub(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	id, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	loaded, err := Load(dir, src)
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}

	if loaded.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: %s vs %s", loaded.Fingerprint, id.Fingerprint)
	}
}

func TestRoundTrip_FileTier(t *testing.T) {
	dir := t.TempDir()
	src, err := NewMasterKeySource(KeySourceOpts{
		ConfigDir:         dir,
		KeychainAvailable: func() bool { return false },
	})
	if err != nil {
		t.Fatalf("NewMasterKeySource: %v", err)
	}

	id, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	loaded, err := Load(dir, src)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.Fingerprint != id.Fingerprint {
		t.Fatalf("fingerprint mismatch: %s vs %s", loaded.Fingerprint, id.Fingerprint)
	}
}

func TestEnvelopeAADBinding(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	path := filepath.Join(dir, identityFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	// Flip a byte in the salt (bytes 8..23 of the 24-byte header).
	data[10] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write tampered file: %v", err)
	}

	_, err = Load(dir, src)
	if err == nil {
		t.Fatal("expected error after tampering header, got nil")
	}
}

func TestFilePermissions(t *testing.T) {
	dir := t.TempDir()
	// Ensure dir has 0750.
	if err := os.Chmod(dir, 0o750); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	src := newTestMasterKeySource(t)
	_, err := LoadOrCreate(dir, src)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	path := filepath.Join(dir, identityFile)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != os.FileMode(0o600) {
		t.Fatalf("expected mode 0600, got %04o", perm)
	}
}

func TestSchemaVersionForwardCompat(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	// Craft a serializedIdentity with a schema_version far in the future.
	id, err := create()
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	s := serializedIdentity{
		SchemaVersion: 99,
		X25519Seed:    id.Keys.X25519Private.Bytes(),
		MLKEMSeed:     id.Keys.MlKemPrivate.Bytes(),
		CreatedAt:     id.CreatedAt,
	}
	jsonBytes, err := json.Marshal(&s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Write it as an envelope.
	salt, _ := kbc.RandomBytes(envelopeSaltSize)
	header := EnvelopeHeader{KDFID: src.KDFID()}
	copy(header.Salt[:], salt)

	perFileKey, err := derivePerFileKey(src, header, "keibidrop-identity-file-v1")
	if err != nil {
		t.Fatalf("derivePerFileKey: %v", err)
	}

	blob, err := kbc.EncryptWithAAD(perFileKey, jsonBytes, header.AAD())
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	copy(header.Nonce[:], blob[:kbc.NonceSize])
	ctAndTag := blob[kbc.NonceSize:]

	path := filepath.Join(dir, identityFile)
	if err := WriteFileAtomic(path, MarshalEnvelope(header, ctAndTag), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = Load(dir, src)
	if !errors.Is(err, ErrIdentityNewerSchema) {
		t.Fatalf("expected ErrIdentityNewerSchema, got: %v", err)
	}
}

func TestKeySubstitution_DifferentMasterKeyFails(t *testing.T) {
	dirA := t.TempDir()
	srcA := newTestMasterKeySource(t)

	_, err := LoadOrCreate(dirA, srcA)
	if err != nil {
		t.Fatalf("LoadOrCreate (A): %v", err)
	}

	// Different master key — derive a different source.
	srcB := &stubMasterKeySource{
		key: []byte("other-master-key-32-bytes-exactly"),
	}

	_, err = Load(dirA, srcB)
	if err == nil {
		t.Fatal("expected error when loading with wrong master key, got nil")
	}

	var ice *IdentityCorruptedError
	if !errors.As(err, &ice) {
		t.Fatalf("expected *IdentityCorruptedError, got %T: %v", err, err)
	}

	// Original file must remain in place (no auto-rename).
	if _, statErr := os.Stat(filepath.Join(dirA, identityFile)); os.IsNotExist(statErr) {
		t.Fatal("original identity.enc must remain in place (no auto-rename)")
	}
}
