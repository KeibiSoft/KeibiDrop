// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 KeibiSoft S.R.L.

// ABOUTME: The relay credit protocol every KeibiDrop client shares: the pasteable code, its hash
// ABOUTME: chain, the paid bridge preamble and ack, the purchase claim and its collect schedule.

// Package tokens holds no state and imports only the standard library, so the
// browser build, which cannot take pkg/logic/common, runs the same code as the
// desktop. pkg/logic/common keeps the wallet, the session and the reveal loop.
package tokens

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// UnitBytes is the bandwidth one chain step buys. PROTOCOL CONSTANT shared with
// the relay ledger, the bridge and the token service.
const UnitBytes int64 = 10 << 20

// ServiceBase is the token service. Prices live on its buy page and in its pack
// table, never in a client.
const ServiceBase = "https://tokens.keibidrop.com" //nolint:gosec // G101: URL, not a credential

// BuyURL is the buy page without a claim: what a person can type or paste.
const BuyURL = ServiceBase + "/buy"

// MaxChainUnits bounds the units a code may carry.
const MaxChainUnits = 1_000_000

// payMagic opens the funded bridge preamble: magic, version, anchor, tier,
// then the room token. Wire format shared with the bridge.
const payMagic = "\x01KDPAY1"

// The bits of the bridge's one ack byte.
const (
	AckPaid       = 1 << 0 // the bridge took the anchor: the pair has priority
	AckContention = 1 << 1 // the bridge is busy: the free lane is being shared
)

// ChainHash is the chain value at position: seed hashed position times. The
// anchor is the value at the chain's full length; spending reveals values
// toward the seed.
func ChainHash(seed [32]byte, position int) [32]byte {
	cur := seed
	for i := 0; i < position; i++ {
		cur = sha256.Sum256(cur[:])
	}
	return cur
}

// DecodeCode validates a pasted "KDT1." code (seed, units, checksum).
func DecodeCode(code string) (seed [32]byte, units int, err error) {
	body, ok := strings.CutPrefix(strings.TrimSpace(code), "KDT1.")
	if !ok {
		return seed, 0, fmt.Errorf("not a KeibiDrop token code (expected \"KDT1.\" prefix)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(raw) != 40 {
		return seed, 0, fmt.Errorf("malformed token code")
	}
	sum := sha256.Sum256(raw[:36])
	if !bytes.Equal(sum[:4], raw[36:]) {
		return seed, 0, fmt.Errorf("token code checksum mismatch (typo?)")
	}
	copy(seed[:], raw[:32])
	units = int(binary.BigEndian.Uint32(raw[32:36]))
	if units < 1 || units > MaxChainUnits {
		return seed, 0, fmt.Errorf("token code units out of range")
	}
	return seed, units, nil
}

// EncodeCode is the pasteable form of a chain.
func EncodeCode(seed [32]byte, units int) string {
	var be [4]byte
	binary.BigEndian.PutUint32(be[:], uint32(units)) // #nosec G115 -- units <= MaxChainUnits
	sum := sha256.Sum256(append(seed[:], be[:]...))
	raw := make([]byte, 0, 40)
	raw = append(raw, seed[:]...)
	raw = append(raw, be[:]...)
	raw = append(raw, sum[:4]...)
	return "KDT1." + base64.RawURLEncoding.EncodeToString(raw)
}

// PayPreamble assembles magic || ver || anchor || tier || token, the single
// write a funded native leg sends where a free one sends the bare token.
func PayPreamble(anchor [32]byte, token [32]byte) []byte {
	pre := make([]byte, 0, len(payMagic)+1+32+1+32)
	pre = append(pre, payMagic...)
	pre = append(pre, 1)
	pre = append(pre, anchor[:]...)
	pre = append(pre, 0) // tier: reserved
	pre = append(pre, token[:]...)
	return pre
}

// maxSurface bounds the surface name a claim carries.
const maxSurface = 12

// NewClaim is a fresh one-shot purchase ref: the surface that started the
// purchase ("desktop", "web", "kd", "cli", "mcp"), an underscore, then 16
// random bytes in base64url. The token service stores the claim with the
// purchase and Stripe shows it as the payment's client_reference_id, so every
// sale says where it was made. Both accept letters, digits, '-' and '_' only,
// and at most 200 characters; the surface is cut to lower-case letters and
// digits so the claim always fits. An empty surface gives the bare random ref
// that clients before 0.5.0 send.
func NewClaim(surface string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	ref := base64.RawURLEncoding.EncodeToString(b[:])
	if s := cleanSurface(surface); s != "" {
		return s + "_" + ref, nil
	}
	return ref, nil
}

func cleanSurface(s string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(s) {
		if out.Len() == maxSurface {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// ClaimSurface is the surface a claim names, or "" for a bare ref. A bare ref
// is 22 base64url characters, which may contain '_' themselves, so only a
// claim longer than that has a surface.
func ClaimSurface(claim string) string {
	const bare = 22
	if len(claim) <= bare+1 || claim[len(claim)-bare-1] != '_' {
		return ""
	}
	return claim[:len(claim)-bare-1]
}

// BuyURLFor is the buy page that carries claim onto every payment link.
func BuyURLFor(base, claim string) string {
	return base + "/buy?claim=" + claim
}

// CollectURL is where the purchase made under claim is picked up as JSON.
func CollectURL(base, claim string) string {
	return base + "/collect?claim=" + claim
}

// The collect schedule. A payment usually lands within two minutes of the buy
// page opening, so the first two minutes poll at the tick; after that the
// person is likely still typing card details, or gone. CollectWindow ends it:
// past it the code still waits on the token service, and on the page Stripe
// returns to, for a paste.
const (
	CollectTick   = 3 * time.Second
	CollectWindow = 30 * time.Minute
)

// CollectDelay is the wait before the next collect poll for a purchase opened
// age ago, on a schedule scaled by tick (CollectTick in production): the tick
// for two minutes, three ticks until ten, then ten ticks. An unpaid claim costs
// the token service about 130 polls over the window instead of 600.
func CollectDelay(age, tick time.Duration) time.Duration {
	switch {
	case age < 2*time.Minute:
		return tick
	case age < 10*time.Minute:
		return 3 * tick
	default:
		return 10 * tick
	}
}
