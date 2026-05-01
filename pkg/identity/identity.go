// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

const identityFile = "identity.enc"

type DeviceIdentity struct {
	Fingerprint string
	Keys        *kbc.OwnKeys
	CreatedAt   time.Time
}

// serializedIdentity is the JSON-serializable form of DeviceIdentity.
// Private key seeds are stored; public keys are derived on load.
type serializedIdentity struct {
	X25519Seed []byte    `json:"x25519_seed"`
	MLKEMSeed  []byte    `json:"mlkem_seed"`
	CreatedAt  time.Time `json:"created_at"`
}

// LoadOrCreate loads an existing identity from configDir, or creates and
// persists a new one if none exists.
func LoadOrCreate(configDir string) (*DeviceIdentity, error) {
	id, err := Load(configDir)
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
	if err := id.Save(configDir); err != nil {
		return nil, fmt.Errorf("save identity: %w", err)
	}
	return id, nil
}

// Load reads and decrypts the identity file from configDir.
func Load(configDir string) (*DeviceIdentity, error) {
	path := filepath.Join(configDir, identityFile)
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	fileKey, err := deriveFileKey()
	if err != nil {
		return nil, fmt.Errorf("derive file key: %w", err)
	}

	plaintext, err := kbc.Decrypt(fileKey, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt identity: %w", err)
	}

	var s serializedIdentity
	if err := json.Unmarshal(plaintext, &s); err != nil {
		return nil, fmt.Errorf("unmarshal identity: %w", err)
	}

	return fromSerialized(&s)
}

// Save encrypts and writes the identity to configDir.
func (d *DeviceIdentity) Save(configDir string) error {
	s := serializedIdentity{
		X25519Seed: d.Keys.X25519Private.Bytes(),
		MLKEMSeed:  d.Keys.MlKemPrivate.Bytes(),
		CreatedAt:  d.CreatedAt,
	}

	plaintext, err := json.Marshal(&s)
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	fileKey, err := deriveFileKey()
	if err != nil {
		return fmt.Errorf("derive file key: %w", err)
	}

	ciphertext, err := kbc.Encrypt(fileKey, plaintext)
	if err != nil {
		return fmt.Errorf("encrypt identity: %w", err)
	}

	if err := os.MkdirAll(configDir, 0750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	path := filepath.Join(configDir, identityFile)
	return os.WriteFile(path, ciphertext, 0600)
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

// deriveFileKey derives a 32-byte encryption key from machine-specific entropy.
// Uses hostname + username + fixed label via HKDF.
func deriveFileKey() ([]byte, error) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown-host"
	}
	u, _ := user.Current()
	username := "unknown-user"
	if u != nil {
		username = u.Username
	}

	ikm := []byte(hostname + ":" + username)
	return kbc.DeriveIdentityFileKey(ikm)
}
