// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package identity

import (
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// Address book add, remove, list and persistence

func TestAddAndLookup(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)
	require.NoError(t, ab.Add("Alice", "fp-alice-1234"))

	c := ab.Lookup("fp-alice-1234")
	require.NotNil(t, c)
	require.Equal(t, "Alice", c.Name)
}

func TestAddDuplicate(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	require.NoError(t, ab.Add("Alice", "fp-alice-1234"))
	require.Error(t, ab.Add("Alice2", "fp-alice-1234"), "expected error on duplicate fingerprint")
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	_ = ab.Add("Alice", "fp-alice")
	_ = ab.Add("Bob", "fp-bob")

	require.NoError(t, ab.Remove("fp-alice"))
	require.Nil(t, ab.Lookup("fp-alice"), "Alice should be removed")
	require.NotNil(t, ab.Lookup("fp-bob"), "Bob should still exist")
	require.Equal(t, 1, ab.Count())
}

func TestRemoveNotFound(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	require.Error(t, ab.Remove("nonexistent"), "expected error removing nonexistent contact")
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	_ = ab.Add("Alice", "fp-alice")
	_ = ab.Add("Bob", "fp-bob")

	contacts := ab.List()
	require.Len(t, contacts, 2)

	// Verify it's a copy (mutations don't affect the address book).
	contacts[0].Name = "MUTATED"
	original := ab.Lookup("fp-alice")
	require.NotEqual(t, "MUTATED", original.Name, "List returned a reference, not a copy")
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		// Create and populate.
		ab1 := testkit.Must(LoadAddressBook(dir, src))
		_ = ab1.Add("Alice", "fp-alice")
		_ = ab1.Add("Bob", "fp-bob")
		require.NoError(t, ab1.Save())

		// Reload from disk.
		ab2 := testkit.Must(LoadAddressBook(dir, src))
		require.Equal(t, 2, ab2.Count())

		c := ab2.Lookup("fp-alice")
		require.True(t, c != nil && c.Name == "Alice", "Alice not found after reload")
		return nil
	})
}

func TestUpdateLastSeen(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	_ = ab.Add("Alice", "fp-alice")

	c := ab.Lookup("fp-alice")
	require.True(t, c.LastSeen.IsZero(), "LastSeen should be zero initially")

	ab.UpdateLastSeen("fp-alice")

	c = ab.Lookup("fp-alice")
	require.False(t, c.LastSeen.IsZero(), "LastSeen should be set after update")
}

func TestEmptyAddressBook(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)
	ab, err := LoadAddressBook(dir, src)
	require.NoError(t, err)

	require.Equal(t, 0, ab.Count())
	require.Nil(t, ab.Lookup("anything"), "Lookup should return nil on empty book")
	require.Empty(t, ab.List())
}

// Contacts v2 schema round trip

func TestContactsV2RoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := newTestMasterKeySource(t)

	testkit.Run(t, func() error {
		ab := testkit.Must(LoadAddressBook(dir, src))
		_ = ab.Add("Charlie", "fp-charlie")
		require.NoError(t, ab.Save())

		ab2 := testkit.Must(LoadAddressBook(dir, src))
		c := ab2.Lookup("fp-charlie")
		require.True(t, c != nil && c.Name == "Charlie", "contact not found after v2 round-trip")
		return nil
	})
}
