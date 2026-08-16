// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for KeibiDrop.EnablePersistentIdentity and related EnableOpts.
// ABOUTME: Covers default, corruption, and passphrase-tier paths.

package common

import (
	"bytes"
	"context"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestKD returns a minimal KeibiDrop instance for testing.
// Port 0 lets the OS assign a free port, avoiding conflicts between parallel tests.
func newTestKD(t *testing.T) *KeibiDrop {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	relay, _ := url.Parse("http://127.0.0.1:54321")
	ctx := context.Background()
	kd, err := NewKeibiDrop(ctx, logger, false, relay, 0, 0, t.TempDir(), t.TempDir(), false, false)
	require.NoError(t, err, "NewKeibiDrop")
	t.Cleanup(func() { kd.Shutdown() })
	return kd
}

func TestEnablePersistentIdentity_DefaultOpts(t *testing.T) {
	kd := newTestKD(t)
	configDir := t.TempDir()

	require.NoError(t, kd.EnablePersistentIdentity(configDir, EnableOpts{}))

	require.NotNil(t, kd.Identity, "expected Identity to be set")
	require.NotEmpty(t, kd.Identity.Fingerprint)
	require.NotNil(t, kd.AddressBook, "expected AddressBook to be set")

	idFile := filepath.Join(configDir, "identity.enc")
	_, err := os.Stat(idFile)
	require.False(t, os.IsNotExist(err), "expected identity.enc to exist on disk")
}

func writeCorruptedIdentity(t *testing.T, configDir string) {
	t.Helper()
	// Build a buffer: magic "KDID" + format 0x01 + kdf_id 2 (file tier) +
	// flags 0x00 + kdf_param 0x00 + 16-byte salt + 12-byte nonce + 32-byte
	// garbage ciphertext. Total = 24+12+32 = 68 bytes.
	buf := make([]byte, 68)
	copy(buf[0:4], "KDID")
	buf[4] = 0x01 // format v1
	buf[5] = 0x02 // KDFFile
	// rest is zeros (salt, nonce) + garbage ct (all zeros, AEAD tag mismatch)
	idFile := filepath.Join(configDir, "identity.enc")
	require.NoError(t, os.WriteFile(idFile, buf, 0o600))
}

func TestEnablePersistentIdentity_CorruptedReturnsTypedError(t *testing.T) {
	kd := newTestKD(t)
	configDir := t.TempDir()

	writeCorruptedIdentity(t, configDir)

	err := kd.EnablePersistentIdentity(configDir, EnableOpts{})
	require.Error(t, err, "expected error for corrupted identity")

	// Identity must not be set.
	require.Nil(t, kd.Identity, "expected Identity to remain nil when corrupted")

	// The original file must still be at its original path (no auto-rename).
	idFile := filepath.Join(configDir, "identity.enc")
	_, err = os.Stat(idFile)
	require.False(t, os.IsNotExist(err), "original identity.enc must remain in place (no auto-rename)")
}

func TestEnablePersistentIdentity_PassphraseTier(t *testing.T) {
	kd := newTestKD(t)
	configDir := t.TempDir()

	opts := EnableOpts{
		PassphraseProtect:  true,
		PassphraseProvider: func() (string, error) { return "test-passphrase", nil },
	}
	require.NoError(t, kd.EnablePersistentIdentity(configDir, opts), "EnablePersistentIdentity passphrase tier")
	require.NotNil(t, kd.Identity, "expected Identity to be set")

	// Read the envelope from disk and verify it carries the Argon2id kdf_id (3).
	idFile := filepath.Join(configDir, "identity.enc")
	buf, err := os.ReadFile(idFile)
	require.NoError(t, err, "read identity.enc")

	require.True(t, bytes.HasPrefix(buf, []byte("KDID")), "identity.enc missing KDID magic")
	// Byte 5 is kdf_id in the envelope layout.
	kdfID := buf[5]
	const kdfPassphrase = 3
	require.Equal(t, byte(kdfPassphrase), kdfID, "expected passphrase kdf_id")
}
