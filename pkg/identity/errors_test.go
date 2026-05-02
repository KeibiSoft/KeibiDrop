// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: Tests for identity sentinel errors and IdentityCorruptedError.
// ABOUTME: Verifies Unwrap, Error string content, and sentinel distinctness.

package identity

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIdentityCorruptedError_Unwrap(t *testing.T) {
	req := require.New(t)
	original := errors.New("disk read failed")
	wrapped := &IdentityCorruptedError{Path: "/some/path", Original: original}

	req.True(errors.Is(wrapped, original), "errors.Is must find the original via Unwrap")
}

func TestIdentityCorruptedError_FormatsPath(t *testing.T) {
	req := require.New(t)
	wrapped := &IdentityCorruptedError{
		Path:     "/etc/keibidrop/identity.enc",
		Original: errors.New("bad data"),
	}
	req.Contains(wrapped.Error(), "/etc/keibidrop/identity.enc",
		"error string must include the file path")
}

func TestIdentityCorruptedError_IsUserFriendly(t *testing.T) {
	req := require.New(t)
	wrapped := &IdentityCorruptedError{
		Path:     "/home/user/.config/keibidrop/identity.enc",
		Original: errors.New("chacha20poly1305: message authentication failed"),
	}
	msg := wrapped.Error()
	req.Contains(msg, "could not be read", "error must contain user-readable description")
	req.Contains(msg, "delete", "error must mention deletion for recovery")
	req.Contains(msg, "Original error:", "error must include the original error")
}

func TestSentinelsAreDistinct(t *testing.T) {
	req := require.New(t)

	sentinels := []error{
		ErrIdentityNeedsPassphrase,
		ErrIdentityNewerSchema,
	}

	for i, a := range sentinels {
		for j, b := range sentinels {
			if i == j {
				continue
			}
			req.False(errors.Is(a, b),
				"sentinel %d must not match sentinel %d", i, j)
		}
	}

	// Each sentinel must match itself via errors.Is.
	for _, s := range sentinels {
		req.True(errors.Is(s, s), "sentinel must match itself")
	}
}
