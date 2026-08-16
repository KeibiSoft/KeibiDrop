// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Shared fixtures: loggers, random data, random files, content compare.
// ABOUTME: Replaces the forked helpers spread across the test packages.

package testkit

import (
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/stretchr/testify/require"
)

// DiscardLogger replaces the inline slog.New(slog.NewTextHandler(io.Discard,
// nil)) construction that appears 72 times across 25 files.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// DebugLogger writes to stderr at debug level.
// Use only while debugging one test. Do not leave it in committed code.
func DebugLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// RandBytes returns n cryptographically random bytes.
func RandBytes(t testing.TB, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}

// RandFile writes size random bytes to dir/name.
// It returns the path and the bytes, so a later read is compared against the
// original without reading the source file twice.
func RandFile(t testing.TB, dir, name string, size int) (string, []byte) {
	t.Helper()
	data := RandBytes(t, size)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path, data
}

// SameContent compares two byte slices and names the first difference.
// It replaces the inlined MD5 compare and its repeated nosec annotation.
// The old sites hashed only to shorten the failure message; fp.BytesEqual
// already reports lengths and an offset instead of the payload.
func SameContent(what string, got, want []byte) error {
	return fp.BytesEqual(what, got, want)
}

// ReadFile reads path and fails the test on error.
func ReadFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //#nosec G304 -- test-controlled path
	require.NoError(t, err)
	return b
}
