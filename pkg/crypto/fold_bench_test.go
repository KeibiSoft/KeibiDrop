// ABOUTME: Micro-benchmarks for the ephemeral hybrid-KEM fold crypto, proving the per-round
// ABOUTME: cost (ML-KEM-1024 keygen, encapsulate, X25519, HKDF combine) is tens of microseconds.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/rand"
	"testing"
)

func benchFoldSalt(b *testing.B) []byte {
	b.Helper()
	salt := make([]byte, minFoldSaltSize)
	if _, err := rand.Read(salt); err != nil {
		b.Fatal(err)
	}
	return salt
}

// BenchmarkFold_InitiatorKeygen isolates the initiator's ephemeral ML-KEM-1024 + X25519
// keygen, the dominant per-fold cost.
func BenchmarkFold_InitiatorKeygen(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fi, err := NewFoldInitiator()
		if err != nil {
			b.Fatal(err)
		}
		_ = fi
	}
}

// BenchmarkFold_Respond measures the responder's work: validate the initiator's publics,
// generate an ephemeral X25519, encapsulate to the ML-KEM public, and combine.
func BenchmarkFold_Respond(b *testing.B) {
	salt := benchFoldSalt(b)
	fi, err := NewFoldInitiator()
	if err != nil {
		b.Fatal(err)
	}
	mlkemPub, x25519Pub := fi.MLKEMPublic(), fi.X25519Public()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, _, err := EphemeralFoldRespond(mlkemPub, x25519Pub, salt); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFold_FullRound measures the entire per-fold crypto cost end to end: initiator
// keygen, responder encapsulate, initiator decapsulate and combine. The RTT must dwarf this
// for the "adds about one round trip, negligible CPU" claim to hold.
func BenchmarkFold_FullRound(b *testing.B) {
	salt := benchFoldSalt(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fi, err := NewFoldInitiator()
		if err != nil {
			b.Fatal(err)
		}
		secret, ct, respPub, err := EphemeralFoldRespond(fi.MLKEMPublic(), fi.X25519Public(), salt)
		if err != nil {
			b.Fatal(err)
		}
		got, err := fi.Derive(ct, respPub, salt)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != KeySize || len(secret) != KeySize {
			b.Fatalf("fold secret length: got %d and %d, want %d", len(got), len(secret), KeySize)
		}
	}
}

// BenchmarkRatchetKeys_Plain and BenchmarkRatchetKeys_Folded bracket the folded ratchet's
// marginal cost: a folded epoch bump only appends the 32-byte secret under two extra HKDF
// labels, so the two should differ by a hair, confirming the fold adds no data-path stall.
func BenchmarkRatchetKeys_Plain(b *testing.B) {
	ck := benchFoldSalt(b) // any 32-byte chain key
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RatchetKeys(ck, 0x01, 1, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRatchetKeys_Folded(b *testing.B) {
	ck := benchFoldSalt(b)
	fold := benchFoldSalt(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := RatchetKeys(ck, 0x01, 1, fold); err != nil {
			b.Fatal(err)
		}
	}
}
