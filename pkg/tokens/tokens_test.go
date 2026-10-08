// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

package tokens

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func seed(b byte) [32]byte {
	var s [32]byte
	for i := range s {
		s[i] = b + byte(i)
	}
	return s
}

// flip changes the character at i to another base64url character.
func flip(code string, i int) string {
	c := byte('A')
	if code[i] == 'A' {
		c = 'B'
	}
	return code[:i] + string(c) + code[i+1:]
}

func TestCodeRoundTrip(t *testing.T) {
	s := seed(7)
	code := EncodeCode(s, 25600)
	got, units, err := DecodeCode("  " + code + "\n")
	if err != nil || got != s || units != 25600 {
		t.Fatalf("round trip: seed ok=%v units=%d err=%v", got == s, units, err)
	}
	for _, bad := range []string{
		"", "KDT2." + strings.TrimPrefix(code, "KDT1."), "KDT1.", "KDT1.!!!!",
		flip(code, 10), // checksum
		EncodeCode(s, 0), EncodeCode(s, MaxChainUnits+1),
	} {
		if _, _, err := DecodeCode(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestChainHashIsRepeatedSHA256(t *testing.T) {
	s := seed(1)
	if ChainHash(s, 0) != s {
		t.Fatal("position 0 must be the seed")
	}
	one := sha256.Sum256(s[:])
	two := sha256.Sum256(one[:])
	if ChainHash(s, 1) != one || ChainHash(s, 2) != two {
		t.Fatal("chain values are not repeated SHA-256")
	}
}

// The preamble layout is the bridge's readPreamble, byte for byte.
func TestPayPreambleLayout(t *testing.T) {
	anchor, token := seed(3), seed(9)
	pre := PayPreamble(anchor, token)
	if len(pre) != 7+1+32+1+32 {
		t.Fatalf("length %d", len(pre))
	}
	if string(pre[:7]) != "\x01KDPAY1" || pre[7] != 1 || pre[8+32] != 0 {
		t.Fatalf("header %x", pre[:9])
	}
	if [32]byte(pre[8:40]) != anchor || [32]byte(pre[41:]) != token {
		t.Fatal("anchor or token misplaced")
	}
}

func TestNewClaimNamesTheSurface(t *testing.T) {
	for _, tc := range []struct{ surface, want string }{
		{"web", "web"},
		{"desktop", "desktop"},
		{"Desktop App!", "desktopapp"},
		{"a-very-long-surface-name", "averylongsur"},
		{"", ""},
		{"___", ""},
	} {
		claim, err := NewClaim(tc.surface)
		if err != nil {
			t.Fatal(err)
		}
		if got := ClaimSurface(claim); got != tc.want {
			t.Errorf("NewClaim(%q) = %q, surface %q, want %q", tc.surface, claim, got, tc.want)
		}
		// What the token service and Stripe accept.
		if len(claim) < 1 || len(claim) > 200 {
			t.Errorf("claim %q length %d", claim, len(claim))
		}
		for _, r := range claim {
			if !claimRune(r) {
				t.Errorf("claim %q has %q", claim, r)
			}
		}
	}
	a, _ := NewClaim("web")
	b, _ := NewClaim("web")
	if a == b {
		t.Fatal("two claims are equal")
	}
}

// claimRune is a character the token service and Stripe accept in a claim.
func claimRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		return true
	}
	return false
}

// A bare ref from a client before 0.5.0 may contain '_' anywhere: it never
// reads as a surface.
func TestClaimSurfaceOfABareRef(t *testing.T) {
	for i := 0; i < 64; i++ {
		var b [16]byte
		for j := range b {
			b[j] = byte(i*16 + j)
		}
		ref := base64.RawURLEncoding.EncodeToString(b[:])
		if got := ClaimSurface(ref); got != "" {
			t.Fatalf("bare ref %q read as surface %q", ref, got)
		}
	}
	if got := ClaimSurface("x_" + strings.Repeat("A", 10)); got != "" {
		t.Fatalf("short claim read as surface %q", got)
	}
}

func TestURLs(t *testing.T) {
	if got := BuyURLFor(ServiceBase, "web_x"); got != "https://tokens.keibidrop.com/buy?claim=web_x" {
		t.Fatal(got)
	}
	if got := CollectURL(ServiceBase, "web_x"); got != "https://tokens.keibidrop.com/collect?claim=web_x" {
		t.Fatal(got)
	}
	if BuyURL != "https://tokens.keibidrop.com/buy" {
		t.Fatal(BuyURL)
	}
}

// The schedule backs off, never polls faster than the tick, and costs an
// unpaid claim a bounded number of polls over the window.
func TestCollectDelaySchedule(t *testing.T) {
	tick := CollectTick
	if CollectDelay(0, tick) != tick || CollectDelay(119*time.Second, tick) != tick {
		t.Fatal("first two minutes must poll at the tick")
	}
	if CollectDelay(2*time.Minute, tick) != 3*tick || CollectDelay(9*time.Minute, tick) != 3*tick {
		t.Fatal("two to ten minutes must poll at three ticks")
	}
	if CollectDelay(10*time.Minute, tick) != 10*tick || CollectDelay(29*time.Minute, tick) != 10*tick {
		t.Fatal("after ten minutes must poll at ten ticks")
	}
	polls := 0
	for age := time.Duration(0); age < CollectWindow; polls++ {
		age += CollectDelay(age, tick)
	}
	if polls > 140 {
		t.Fatalf("an unpaid claim costs %d polls", polls)
	}
}
