// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Encrypted address book (contacts list) for the identity package.
// ABOUTME: Loads, saves, and manages contacts using the same envelope format as identities.

package identity

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

const contactsFile = "contacts.enc"

type Contact struct {
	SchemaVersion int       `json:"schema_version,omitempty"`
	Name          string    `json:"name"`
	Fingerprint   string    `json:"fingerprint"`
	AddedAt       time.Time `json:"added_at"`
	LastSeen      time.Time `json:"last_seen,omitempty"`
}

type AddressBook struct {
	mu        sync.RWMutex
	contacts  []Contact
	configDir string
	src       MasterKeySource
}

// LoadAddressBook loads the encrypted address book from configDir using src.
// Returns an empty address book if the file does not exist.
func LoadAddressBook(configDir string, src MasterKeySource) (*AddressBook, error) {
	ab := &AddressBook{configDir: configDir, src: src}

	path := filepath.Join(configDir, contactsFile)

	buf, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return ab, nil // empty address book
		}
		return nil, fmt.Errorf("read contacts: %w", readErr)
	}

	if !IsV1Envelope(buf) {
		return nil, fmt.Errorf("contacts %s: not a KDID envelope", path)
	}

	header, ctAndTag, parseErr := ParseEnvelope(buf)
	if parseErr != nil {
		return nil, fmt.Errorf("contacts %s: %w", path, parseErr)
	}

	perFileKey, keyErr := derivePerFileKey(src, header, "keibidrop-contacts-file-v1")
	if keyErr != nil {
		return nil, fmt.Errorf("contacts %s: derive key: %w", path, keyErr)
	}

	blob := make([]byte, kbc.NonceSize+len(ctAndTag))
	copy(blob[:kbc.NonceSize], header.Nonce[:])
	copy(blob[kbc.NonceSize:], ctAndTag)

	plaintext, decErr := kbc.DecryptWithAAD(perFileKey, blob, header.AAD())
	if decErr != nil {
		return nil, fmt.Errorf("contacts %s: decrypt: %w", path, decErr)
	}

	if jsonErr := json.Unmarshal(plaintext, &ab.contacts); jsonErr != nil {
		return nil, fmt.Errorf("contacts %s: unmarshal: %w", path, jsonErr)
	}

	return ab, nil
}

// Save encrypts and persists the address book to disk using the MasterKeySource
// that was provided to LoadAddressBook.
func (ab *AddressBook) Save() error {
	ab.mu.RLock()
	data, err := json.Marshal(ab.contacts)
	ab.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal contacts: %w", err)
	}

	if err := os.MkdirAll(ab.configDir, 0o750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	salt, err := kbc.RandomBytes(envelopeSaltSize)
	if err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}

	header := EnvelopeHeader{KDFID: ab.src.KDFID()}
	copy(header.Salt[:], salt)

	perFileKey, err := derivePerFileKey(
		ab.src, header, "keibidrop-contacts-file-v1",
	)
	if err != nil {
		return fmt.Errorf("derive per-file key: %w", err)
	}

	blob, err := kbc.EncryptWithAAD(perFileKey, data, header.AAD())
	if err != nil {
		return fmt.Errorf("encrypt contacts: %w", err)
	}

	copy(header.Nonce[:], blob[:kbc.NonceSize])
	ctAndTag := blob[kbc.NonceSize:]

	path := filepath.Join(ab.configDir, contactsFile)
	return WriteFileAtomic(path, MarshalEnvelope(header, ctAndTag), 0o600)
}

// Add adds a contact. Returns error if fingerprint already exists.
func (ab *AddressBook) Add(name, fingerprint string) error {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for _, c := range ab.contacts {
		if c.Fingerprint == fingerprint {
			return fmt.Errorf(
				"contact with fingerprint %s already exists", fingerprint[:8],
			)
		}
	}

	ab.contacts = append(ab.contacts, Contact{
		Name:        name,
		Fingerprint: fingerprint,
		AddedAt:     time.Now(),
	})
	return nil
}

// Remove removes a contact by fingerprint.
func (ab *AddressBook) Remove(fingerprint string) error {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for i, c := range ab.contacts {
		if c.Fingerprint == fingerprint {
			ab.contacts = append(ab.contacts[:i], ab.contacts[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("contact not found")
}

// Lookup returns a contact by fingerprint, or nil if not found.
func (ab *AddressBook) Lookup(fingerprint string) *Contact {
	ab.mu.RLock()
	defer ab.mu.RUnlock()

	for i := range ab.contacts {
		if ab.contacts[i].Fingerprint == fingerprint {
			return &ab.contacts[i]
		}
	}
	return nil
}

// UpdateLastSeen updates the last seen time for a contact.
func (ab *AddressBook) UpdateLastSeen(fingerprint string) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for i := range ab.contacts {
		if ab.contacts[i].Fingerprint == fingerprint {
			ab.contacts[i].LastSeen = time.Now()
			return
		}
	}
}

// List returns a copy of all contacts.
func (ab *AddressBook) List() []Contact {
	ab.mu.RLock()
	defer ab.mu.RUnlock()

	out := make([]Contact, len(ab.contacts))
	copy(out, ab.contacts)
	return out
}

// Count returns the number of contacts.
func (ab *AddressBook) Count() int {
	ab.mu.RLock()
	defer ab.mu.RUnlock()
	return len(ab.contacts)
}

// ConfigDir returns the configDir the address book was loaded from.
func (ab *AddressBook) ConfigDir() string { return ab.configDir }
