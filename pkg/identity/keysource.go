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

// Tier names the active master-key strategy. Each MasterKeySource returns
// exactly one of the package-level Tier constants.
type Tier string

const (
	TierKeychain   Tier = "keychain"   // OS keychain (Tier 1a, desktop default)
	TierFile       Tier = "file"       // ~/.config/keibidrop/.master.key (Tier 1b, headless)
	TierPassphrase Tier = "passphrase" // user passphrase (Tier 2, opt-in)
	TierExternal   Tier = "external"   // FFI-injected master from mobile native
)

// MasterKeySource provides a stable 32-byte master key for this install or
// session. Implementations cover three security tiers:
//   - Tier 1a: OS keychain (most secure)
//   - Tier 1b: file on disk (~/.config/keibidrop/.master.key)
//   - Tier 2:  passphrase (user-supplied, portable across machines)
//   - External: FFI-injected bytes from mobile (iOS/Android) bridge
type MasterKeySource interface {
	// Master returns a stable 32-byte master key for this install/session.
	// Generated lazily on first call; subsequent calls return the same bytes.
	Master() ([]byte, error)
	// Tier identifies the key source for logging and diagnostics.
	Tier() Tier
	// KDFID returns the envelope kdf_id for files written with this source's
	// derived per-file keys (KDFKeychain | KDFFile | KDFPassphrase).
	KDFID() uint8
}

// KeySourceOpts configures NewMasterKeySource.
type KeySourceOpts struct {
	ConfigDir         string
	PassphraseProtect bool
	// PassphraseProvider is called at most once when PassphraseProtect is true.
	PassphraseProvider func() (string, error)
	// KeychainAvailable overrides IsKeychainAvailable for testing.
	// nil means use the real implementation.
	KeychainAvailable func() bool

	// ExternalMaster is a 32-byte master key supplied by the caller from
	// outside the Go core — typically the mobile bridge reading it from
	// iOS Keychain Services (Swift) or Android Keystore (Kotlin). When
	// non-nil and 32 bytes, this overrides desktop tier selection entirely
	// and the resulting MasterKeySource yields these bytes.
	//
	// SECURITY CONTRACT for whoever fills this field (mobile bridges):
	//
	//   1. The 32 bytes MUST come from a CSPRNG. Generate once at first
	//      launch via crypto/rand (or call GenerateMasterKey() below for
	//      a Go-side helper), persist into the platform's secure store,
	//      and read it back from there on subsequent launches.
	//   2. The 32 bytes MUST be unique per install AND persistent across
	//      launches. Hardcoding bytes in the app binary, or deriving them
	//      from device-public information (UUID, MAC, IMEI), would let any
	//      attacker decrypt any user's identity.
	//   3. The 32 bytes MUST live in a platform-secure secret store: iOS
	//      Keychain Services with `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`
	//      + `kSecAttrSynchronizable=false` to keep them out of iCloud
	//      Backup; Android Keystore with `setIsStrongBoxBacked(true)`
	//      opportunistic. Never plaintext files, never SharedPreferences.
	//
	// Violating any of these makes the at-rest encryption useless.
	//
	// On desktop this stays nil; tier 1a (OS keychain via go-keyring),
	// tier 1b (~/.config/keibidrop/.master.key), or tier 2 (passphrase)
	// applies as before.
	ExternalMaster []byte
}

// GenerateMasterKey returns 32 freshly random bytes from crypto/rand.
// Mobile bridges should call this on first launch (when the platform
// secret store has no entry yet), persist the bytes via the platform
// Keystore/Keychain, and pass them back via KeySourceOpts.ExternalMaster
// on every subsequent NewMasterKeySource call. Doing the generation in
// Go avoids the failure mode where a sloppy bridge hardcodes a key,
// uses Java's Math.random(), or derives from device-public info.
func GenerateMasterKey() ([]byte, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("read crypto/rand: %w", err)
	}
	return key, nil
}

// isObviouslyWeakKey rejects the most common bridge-implementation
// footguns: all-zero, all-same-byte, sequential ascending. Not a substitute
// for a CSPRNG — only blocks the trivially-broken cases.
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

// NewMasterKeySource selects the appropriate tier and returns a ready source.
// Selection order:
//  1. opts.ExternalMaster non-nil → externalKeySource (mobile bridge injection)
//  2. PassphraseProtect == true   → passphraseSource (Tier 2)
//  3. keychain available           → keychainSource   (Tier 1a)
//  4. otherwise                    → fileSource       (Tier 1b)
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

// ── externalKeySource ─────────────────────────────────────────────────────────

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

// ── keychainSource ────────────────────────────────────────────────────────────

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
		// Not present — generate and store.
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

// ── fileSource ────────────────────────────────────────────────────────────────

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
		// File not present — generate a new key and write it atomically.
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
		return nil, &IdentityCorruptedError{
			Path: path,
			Original: fmt.Errorf(
				"master key file has wrong length: got %d bytes, want 32",
				len(data),
			),
		}
	}

	s.cache = data
	return s.cache, nil
}

// ── passphraseSource ──────────────────────────────────────────────────────────

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
