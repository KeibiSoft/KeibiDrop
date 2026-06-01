// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Tests for resolveKeyWithFallback — the key-normalisation helper used by GetDownloadProgress.
// ABOUTME: Covers exact-key, slash-prefix fallback, strip-slash fallback, precedence, and not-found.

package common

import "testing"

func TestResolveKeyWithFallback(t *testing.T) {
	t.Run("exact key present", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		if !found {
			t.Fatal("expected found=true, got false")
		}
		if got != "foo" {
			t.Errorf("got key %q, want %q", got, "foo")
		}
	})

	t.Run("slash-prefix key, bare query", func(t *testing.T) {
		m := map[string]struct{}{"/foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		if !found {
			t.Fatal("expected found=true, got false")
		}
		if got != "/foo" {
			t.Errorf("got key %q, want %q", got, "/foo")
		}
	})

	t.Run("bare key, slash-prefix query", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("/foo", exists)
		if !found {
			t.Fatal("expected found=true, got false")
		}
		if got != "foo" {
			t.Errorf("got key %q, want %q", got, "foo")
		}
	})

	t.Run("both present, exact wins", func(t *testing.T) {
		m := map[string]struct{}{"foo": {}, "/foo": {}}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("foo", exists)
		if !found {
			t.Fatal("expected found=true, got false")
		}
		if got != "foo" {
			t.Errorf("got key %q, want %q (exact-key must win)", got, "foo")
		}
	})

	t.Run("unknown key returns not found", func(t *testing.T) {
		m := map[string]struct{}{}
		exists := func(k string) bool { _, ok := m[k]; return ok }
		got, found := resolveKeyWithFallback("bar", exists)
		if found {
			t.Fatalf("expected found=false, got true (key=%q)", got)
		}
		if got != "" {
			t.Errorf("expected empty string on miss, got %q", got)
		}
	})
}
