// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Device identity load/save for the identity package.
// ABOUTME: Manages the on-disk encrypted identity envelope and key lifecycle.

package identity

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

const identityFile = "identity.enc"

// CurrentIdentitySchemaVersion is the schema_version written by this build.
const CurrentIdentitySchemaVersion = 1

type DeviceIdentity struct {
	Fingerprint string
	Keys        *kbc.OwnKeys
	CreatedAt   time.Time
}

// serializedIdentity is the JSON-serializable form of DeviceIdentity.
// Private key seeds are stored; public keys are derived on load.
type serializedIdentity struct {
	SchemaVersion int       `json:"schema_version"`
	X25519Seed    []byte    `json:"x25519_seed"`
	MLKEMSeed     []byte    `json:"mlkem_seed"`
	CreatedAt     time.Time `json:"created_at"`
}

// LoadOrCreate loads an existing identity from configDir using src, or
// creates and persists a new one if none exists.
func LoadOrCreate(configDir string, src MasterKeySource) (*DeviceIdentity, error) {
	id, err := Load(configDir, src)
	if err == nil {
		return id, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	id, err = create()
	if err != nil {
		return nil, fmt.Errorf("create identity: %w", err)
	}
	if err := id.Save(configDir, src); err != nil {
		return nil, fmt.Errorf("save identity: %w", err)
	}
	return id, nil
}

// Load reads and decrypts the identity file from configDir using src.
func Load(configDir string, src MasterKeySource) (*DeviceIdentity, error) {
	path := filepath.Join(configDir, identityFile)

	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if !IsV1Envelope(buf) {
		return nil, &IdentityCorruptedError{
			Path:     path,
			Original: errors.New("file is not a KDID envelope"),
		}
	}

	header, ctAndTag, perr := ParseEnvelope(buf)
	if perr != nil {
		return nil, &IdentityCorruptedError{Path: path, Original: perr}
	}

	perFileKey, kerr := derivePerFileKey(src, header, "keibidrop-identity-file-v1")
	if kerr != nil {
		return nil, fmt.Errorf("derive per-file key: %w", kerr)
	}

	blob := make([]byte, kbc.NonceSize+len(ctAndTag))
	copy(blob[:kbc.NonceSize], header.Nonce[:])
	copy(blob[kbc.NonceSize:], ctAndTag)

	pt, decErr := kbc.DecryptWithAAD(perFileKey, blob, header.AAD())
	if decErr != nil {
		return nil, &IdentityCorruptedError{
			Path:     path,
			Original: fmt.Errorf("decrypt identity: %w", decErr),
		}
	}

	var s serializedIdentity
	if jerr := json.Unmarshal(pt, &s); jerr != nil {
		return nil, &IdentityCorruptedError{
			Path:     path,
			Original: fmt.Errorf("unmarshal identity: %w", jerr),
		}
	}

	if s.SchemaVersion > CurrentIdentitySchemaVersion {
		return nil, ErrIdentityNewerSchema
	}

	return fromSerialized(&s)
}

// Save encrypts and writes the identity to configDir using src.
func (d *DeviceIdentity) Save(configDir string, src MasterKeySource) error {
	s := serializedIdentity{
		SchemaVersion: CurrentIdentitySchemaVersion,
		X25519Seed:    d.Keys.X25519Private.Bytes(),
		MLKEMSeed:     d.Keys.MlKemPrivate.Bytes(),
		CreatedAt:     d.CreatedAt,
	}

	jsonBytes, err := json.Marshal(&s)
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	if err := os.MkdirAll(configDir, 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	salt, err := kbc.RandomBytes(envelopeSaltSize)
	if err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}

	header := EnvelopeHeader{KDFID: src.KDFID()}
	copy(header.Salt[:], salt)

	perFileKey, err := derivePerFileKey(src, header, "keibidrop-identity-file-v1")
	if err != nil {
		return fmt.Errorf("derive per-file key: %w", err)
	}

	blob, err := kbc.EncryptWithAAD(perFileKey, jsonBytes, header.AAD())
	if err != nil {
		return fmt.Errorf("encrypt identity: %w", err)
	}

	// Split [nonce | ct+tag], record nonce in header so it is part of the
	// on-disk wire format (MarshalEnvelope writes it after the 24-byte prefix).
	copy(header.Nonce[:], blob[:kbc.NonceSize])
	ctAndTag := blob[kbc.NonceSize:]

	path := filepath.Join(configDir, identityFile)
	return WriteFileAtomic(path, MarshalEnvelope(header, ctAndTag), 0o600)
}

func create() (*DeviceIdentity, error) {
	kemPriv, kemPub, err := kbc.GenerateMLKEMKeypair()
	if err != nil {
		return nil, fmt.Errorf("generate mlkem keypair: %w", err)
	}

	ecPriv, ecPub, err := kbc.GenerateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("generate x25519 keypair: %w", err)
	}

	keys := &kbc.OwnKeys{
		MlKemPrivate:  kemPriv,
		MlKemPublic:   kemPub,
		X25519Private: ecPriv,
		X25519Public:  ecPub,
	}

	fp, err := keys.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("compute fingerprint: %w", err)
	}

	return &DeviceIdentity{
		Fingerprint: fp,
		Keys:        keys,
		CreatedAt:   time.Now(),
	}, nil
}

func fromSerialized(s *serializedIdentity) (*DeviceIdentity, error) {
	kemPriv, err := mlkem.NewDecapsulationKey1024(s.MLKEMSeed)
	if err != nil {
		return nil, fmt.Errorf("restore mlkem key: %w", err)
	}

	ecPriv, err := ecdh.X25519().NewPrivateKey(s.X25519Seed)
	if err != nil {
		return nil, fmt.Errorf("restore x25519 key: %w", err)
	}

	keys := &kbc.OwnKeys{
		MlKemPrivate:  kemPriv,
		MlKemPublic:   kemPriv.EncapsulationKey(),
		X25519Private: ecPriv,
		X25519Public:  ecPriv.PublicKey(),
	}

	fp, err := keys.Fingerprint()
	if err != nil {
		return nil, fmt.Errorf("compute fingerprint: %w", err)
	}

	return &DeviceIdentity{
		Fingerprint: fp,
		Keys:        keys,
		CreatedAt:   s.CreatedAt,
	}, nil
}

// derivePerFileKey returns the per-file AEAD key for an envelope, using the
// appropriate KDF for the envelope's kdf_id.
func derivePerFileKey(
	src MasterKeySource,
	header EnvelopeHeader,
	info string,
) ([]byte, error) {
	masterKey, err := src.Master()
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}

	switch header.KDFID {
	case KDFKeychain, KDFFile:
		return kbc.DeriveFileEncryptionKey(masterKey, header.Salt[:], info)
	case KDFPassphrase:
		return DerivePassphraseKey(string(masterKey), header.Salt[:])
	default:
		return nil, fmt.Errorf("unknown kdf_id %d", header.KDFID)
	}
}
