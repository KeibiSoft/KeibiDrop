// ABOUTME: Micro-benchmark for staging a fold secret on a SecureConn, the once-per-round
// ABOUTME: per-connection cost the fold driver pays; must be trivial next to the network RTT.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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
