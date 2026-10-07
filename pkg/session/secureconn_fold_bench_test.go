// ABOUTME: Micro-benchmark for staging a fold secret on a SecureConn, the once-per-round
// ABOUTME: per-connection cost the fold driver pays; must be trivial next to the network RTT.

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package session

import (
	"crypto/rand"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

func benchFoldKey(b *testing.B) []byte {
	b.Helper()
	k := make([]byte, kbc.KeySize)
	if _, err := rand.Read(k); err != nil {
		b.Fatal(err)
	}
	return k
}

// Measures staging a fold secret. Staged as a gated responder so it re-stages each iteration
// without consuming the secret or bumping the epoch, isolating the pure staging cost.
func BenchmarkSecureConn_StageEntropyFold(b *testing.B) {
	sc := NewSecureConn(&writeSink{}, benchFoldKey(b), kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetKeyUpdate(true)
	secret := benchFoldKey(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sc.StageEntropyFold(secret, false)
	}
}
