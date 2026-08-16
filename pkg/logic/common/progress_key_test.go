// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Tests for resolveKeyWithFallback — the key-normalisation helper used by GetDownloadProgress.
// ABOUTME: Covers exact-key, slash-prefix fallback, strip-slash fallback, precedence, and not-found.

package common

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveKeyWithFallback(t *testing.T) {
	t.Run("exact key present", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		require.True(t, found)
		require.Equal(t, "foo", got)
	})

	t.Run("slash-prefix key, bare query", func(t *testing.T) {
		m := map[string]struct{}{"/foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		require.True(t, found)
		require.Equal(t, "/foo", got)
	})

	t.Run("bare key, slash-prefix query", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("/foo", exists)
		require.True(t, found)
		require.Equal(t, "foo", got)
	})

	t.Run("both present, exact wins", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}, "/foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		require.True(t, found)
		require.Equal(t, "foo", got, "exact-key must win")
	})

	t.Run("unknown key returns not found", func(t *testing.T) {
		m := map[string]struct{}{}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("bar", exists)
		require.False(t, found)
		require.Empty(t, got, "expected empty string on miss")
	})
}
