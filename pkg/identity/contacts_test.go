// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ── Existing tests (updated to pass MasterKeySource) ─────────────────────────

func TestAddAndLookup(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	if err := ab.Add("Alice", "fp-alice-1234"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	c := ab.Lookup("fp-alice-1234")
	require.NotNil(t, c)
	require.Equal(t, "Alice", c.Name)
}

func TestAddDuplicate(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	if err := ab.Add("Alice", "fp-alice-1234"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := ab.Add("Alice2", "fp-alice-1234"); err == nil {
		t.Fatal("expected error on duplicate fingerprint")
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	_ = ab.Add("Alice", "fp-alice")
	_ = ab.Add("Bob", "fp-bob")

	if err := ab.Remove("fp-alice"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if ab.Lookup("fp-alice") != nil {
		t.Fatal("Alice should be removed")
	}
	if ab.Lookup("fp-bob") == nil {
		t.Fatal("Bob should still exist")
	}
	if ab.Count() != 1 {
		t.Fatalf("expected 1 contact, got %d", ab.Count())
	}
}

func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	if err := ab.Remove("nonexistent"); err == nil {
		t.Fatal("expected error removing nonexistent contact")
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	_ = ab.Add("Alice", "fp-alice")
	_ = ab.Add("Bob", "fp-bob")

	contacts := ab.List()
	if len(contacts) != 2 {
		t.Fatalf("expected 2 contacts, got %d", len(contacts))
	}

	// Verify it's a copy (mutations don't affect the address book).
	contacts[0].Name = "MUTATED"
	original := ab.Lookup("fp-alice")
	if original.Name == "MUTATED" {
		t.Fatal("List returned a reference, not a copy")
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	// Create and populate.
	ab1, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}
	_ = ab1.Add("Alice", "fp-alice")
	_ = ab1.Add("Bob", "fp-bob")
	if err := ab1.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload from disk.
	ab2, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("second LoadAddressBook: %v", err)
	}

	if ab2.Count() != 2 {
		t.Fatalf("expected 2 contacts after reload, got %d", ab2.Count())
	}

	c := ab2.Lookup("fp-alice")
	if c == nil || c.Name != "Alice" {
		t.Fatal("Alice not found after reload")
	}
}

func TestUpdateLastSeen(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	_ = ab.Add("Alice", "fp-alice")

	c := ab.Lookup("fp-alice")
	if !c.LastSeen.IsZero() {
		t.Fatal("LastSeen should be zero initially")
	}

	ab.UpdateLastSeen("fp-alice")

	c = ab.Lookup("fp-alice")
	if c.LastSeen.IsZero() {
		t.Fatal("LastSeen should be set after update")
	}
}

func TestEmptyAddressBook(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}

	if ab.Count() != 0 {
		t.Fatalf("expected 0 contacts, got %d", ab.Count())
	}
	if ab.Lookup("anything") != nil {
		t.Fatal("Lookup should return nil on empty book")
	}
	contacts := ab.List()
	if len(contacts) != 0 {
		t.Fatalf("expected empty list, got %d", len(contacts))
	}
}

// ── New v2 tests ─────────────────────────────────────────────────────────────

func TestContactsV2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	ab, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("LoadAddressBook: %v", err)
	}
	_ = ab.Add("Charlie", "fp-charlie")
	if err := ab.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	ab2, err := LoadAddressBook(dir, src)
	if err != nil {
		t.Fatalf("second LoadAddressBook: %v", err)
	}
	c := ab2.Lookup("fp-charlie")
	if c == nil || c.Name != "Charlie" {
		t.Fatal("contact not found after v2 round-trip")
	}
}
