// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package main

import (
	"testing"
)

func TestListEscapeRoundTrips(t *testing.T) {
	for _, s := range []string{"plain.txt", "tab\there", "line\nbreak", `C:\Users\x`, `trailing\`, "mixed\t\\\n"} {
		if got := listUnescape(listEscape(s)); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
		if e := listEscape(s); len(e) > 0 && (contains(e, '\t') || contains(e, '\n')) {
			t.Errorf("%q escaped to %q still holds a tab or newline", s, e)
		}
	}
}

func contains(s string, c byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return true
		}
	}
	return false
}
