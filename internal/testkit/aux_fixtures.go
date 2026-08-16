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
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"os"
	"testing"
)

// Logger returns a text logger that writes to w at the given level.
// It is the general form. Use it when a test must keep its output, for example
// an integration test that logs warnings to stdout for a human to read after a
// failure. Replacing such a logger with DiscardLogger loses that output.
func Logger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

// DiscardLogger drops all output. It replaces the inline
// slog.New(slog.NewTextHandler(io.Discard, nil)) construction.
// slog defaults to LevelInfo when the options are nil, so this is unchanged
// behaviour for those sites.
func DiscardLogger() *slog.Logger {
	return Logger(io.Discard, slog.LevelInfo)
}

// StdoutLogger writes to stdout at the given level.
// The integration tests use this to keep warnings and errors visible.
func StdoutLogger(level slog.Level) *slog.Logger {
	return Logger(os.Stdout, level)
}

// RandBytes returns n cryptographically random bytes.
func RandBytes(t testing.TB, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	_, err := rand.Read(buf)
	require.NoError(t, err)
	return buf
}
