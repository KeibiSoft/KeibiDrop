// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	AddedAt     time.Time `json:"added_at"`
	LastSeen    time.Time `json:"last_seen,omitempty"`
}

type AddressBook struct {
	mu        sync.RWMutex
	contacts  []Contact
	configDir string
}

// LoadAddressBook loads the encrypted address book from configDir.
// Returns an empty address book if the file doesn't exist.
func LoadAddressBook(configDir string) (*AddressBook, error) {
	ab := &AddressBook{configDir: configDir}

	path := filepath.Join(configDir, contactsFile)
	ciphertext, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ab, nil
		}
		return nil, fmt.Errorf("read contacts: %w", err)
	}

	fileKey, err := deriveFileKey()
	if err != nil {
		return nil, fmt.Errorf("derive file key: %w", err)
	}

	plaintext, err := kbc.Decrypt(fileKey, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("decrypt contacts: %w", err)
	}

	if err := json.Unmarshal(plaintext, &ab.contacts); err != nil {
		return nil, fmt.Errorf("unmarshal contacts: %w", err)
	}

	return ab, nil
}

// Save encrypts and persists the address book to disk.
func (ab *AddressBook) Save() error {
	ab.mu.RLock()
	data, err := json.Marshal(ab.contacts)
	ab.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("marshal contacts: %w", err)
	}

	fileKey, err := deriveFileKey()
	if err != nil {
		return fmt.Errorf("derive file key: %w", err)
	}

	ciphertext, err := kbc.Encrypt(fileKey, data)
	if err != nil {
		return fmt.Errorf("encrypt contacts: %w", err)
	}

	if err := os.MkdirAll(ab.configDir, 0750); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}

	path := filepath.Join(ab.configDir, contactsFile)
	return os.WriteFile(path, ciphertext, 0600)
}

// Add adds a contact. Returns error if fingerprint already exists.
func (ab *AddressBook) Add(name, fingerprint string) error {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	for _, c := range ab.contacts {
		if c.Fingerprint == fingerprint {
			return fmt.Errorf("contact with fingerprint %s already exists", fingerprint[:8])
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
