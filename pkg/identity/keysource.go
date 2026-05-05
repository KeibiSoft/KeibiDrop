// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: MasterKeySource abstraction for the identity package.
// ABOUTME: Selects between keychain, file-based, passphrase, and external key tiers.

package identity

import (
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

type Tier string

const (
	TierKeychain   Tier = "keychain"
	TierFile       Tier = "file"
	TierPassphrase Tier = "passphrase"
	TierExternal   Tier = "external"
)

type MasterKeySource interface {
	Master() ([]byte, error)
	Tier() Tier
	KDFID() uint8
}

type KeySourceOpts struct {
	ConfigDir          string
	PassphraseProtect  bool
	PassphraseProvider func() (string, error)
	KeychainAvailable  func() bool // nil = real check
	ExternalMaster     []byte      // 32-byte key from mobile bridge (iOS Keychain / Android Keystore)
}

func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("read crypto/rand: %w", err)
	}
	return key, nil
}

func isObviouslyWeakKey(key []byte) bool {
	if len(key) != 32 {
		return true
	}
	allSame := true
	sequential := true
	for i := 1; i < len(key); i++ {
		if key[i] != key[0] {
			allSame = false
		}
		if int(key[i]) != int(key[0])+i {
			sequential = false
		}
	}
	return allSame || sequential
}

func NewMasterKeySource(opts KeySourceOpts) (MasterKeySource, error) {
	logger := slog.With("method", "new-master-key-source")

	if opts.ExternalMaster != nil {
		if len(opts.ExternalMaster) != 32 {
			return nil, errors.New("ExternalMaster must be 32 bytes")
		}
		if isObviouslyWeakKey(opts.ExternalMaster) {
			return nil, errors.New("ExternalMaster is obviously weak; refusing")
		}
		out := make([]byte, 32)
		copy(out, opts.ExternalMaster)
		logger.Info("Tier selected", "tier", TierExternal)
		return &externalKeySource{master: out}, nil
	}

	keychainAvail := IsKeychainAvailable
	if opts.KeychainAvailable != nil {
		keychainAvail = opts.KeychainAvailable
	}

	if opts.PassphraseProtect {
		if opts.PassphraseProvider == nil {
			return nil, errors.New(
				"identity: PassphraseProtect requires a PassphraseProvider",
			)
		}
		logger.Info("Tier selected", "tier", TierPassphrase)
		return &passphraseSource{provider: opts.PassphraseProvider}, nil
	}

	if keychainAvail() {
		logger.Info("Tier selected", "tier", TierKeychain)
		return &keychainSource{}, nil
	}

	logger.Info("Tier selected", "tier", TierFile, "configDir", opts.ConfigDir)
	return &fileSource{configDir: opts.ConfigDir}, nil
}

type externalKeySource struct {
	master []byte
}

func (s *externalKeySource) Tier() Tier   { return TierExternal }
func (s *externalKeySource) KDFID() uint8 { return KDFFile }

func (s *externalKeySource) Master() ([]byte, error) {
	out := make([]byte, len(s.master))
	copy(out, s.master)
	return out, nil
}

type keychainSource struct {
	mu    sync.Mutex
	cache []byte
}

func (s *keychainSource) Tier() Tier   { return TierKeychain }
func (s *keychainSource) KDFID() uint8 { return KDFKeychain }

func (s *keychainSource) Master() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil {
		return s.cache, nil
	}

	key, err := KeychainGet(keychainAccountIdentityMasterV1)
	if err != nil {
		// Not present, generate and store.
		newKey, genErr := kbc.RandomBytes(32)
		if genErr != nil {
			return nil, fmt.Errorf(
				"identity keychain: generate master key: %w", genErr,
			)
		}
		if setErr := KeychainSet(keychainAccountIdentityMasterV1, newKey); setErr != nil {
			return nil, fmt.Errorf(
				"identity keychain: store master key: %w", setErr,
			)
		}
		s.cache = newKey
		return s.cache, nil
	}

	if len(key) != 32 {
		return nil, fmt.Errorf(
			"identity keychain: master key wrong length: got %d, want 32", len(key),
		)
	}

	s.cache = key
	return s.cache, nil
}

type fileSource struct {
	configDir string
	mu        sync.Mutex
	cache     []byte
}

func (s *fileSource) Tier() Tier   { return TierFile }
func (s *fileSource) KDFID() uint8 { return KDFFile }

func (s *fileSource) masterPath() string {
	return filepath.Join(s.configDir, ".master.key")
}

func (s *fileSource) Master() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil {
		return s.cache, nil
	}

	path := s.masterPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("identity file source: read %q: %w", path, err)
		}
		// File not present, generate and write atomically.
		newKey, genErr := kbc.RandomBytes(32)
		if genErr != nil {
			return nil, fmt.Errorf(
				"identity file source: generate master key: %w", genErr,
			)
		}
		if writeErr := WriteFileAtomic(path, newKey, 0o600); writeErr != nil {
			return nil, fmt.Errorf(
				"identity file source: write %q: %w", path, writeErr,
			)
		}
		s.cache = newKey
		return s.cache, nil
	}

	if len(data) != 32 {
		return nil, fmt.Errorf("master key %s: wrong length %d, want 32", path, len(data))
	}

	s.cache = data
	return s.cache, nil
}

type passphraseSource struct {
	provider func() (string, error)
	mu       sync.Mutex
	cache    []byte
}

func (s *passphraseSource) Tier() Tier   { return TierPassphrase }
func (s *passphraseSource) KDFID() uint8 { return KDFPassphrase }

// Master returns the passphrase as bytes. Per-file key derivation (in Save/Load)
// passes string(masterBytes) to DerivePassphraseKey so it receives the original
// passphrase text.
func (s *passphraseSource) Master() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cache != nil {
		return s.cache, nil
	}

	pp, err := s.provider()
	if err != nil {
		return nil, fmt.Errorf("identity passphrase source: provider: %w", err)
	}
	if len(pp) == 0 {
		return nil, errors.New("identity passphrase source: empty passphrase")
	}

	s.cache = []byte(pp)
	return s.cache, nil
}
