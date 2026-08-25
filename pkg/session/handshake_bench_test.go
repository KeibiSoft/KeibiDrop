// ABOUTME: Micro-benchmark for a full inbound handshake, the once-per-connection
// ABOUTME: cost that any deadline bookkeeping on that path has to stay trivial against.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"io"
	"log/slog"
	"net"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// BenchmarkInboundHandshake times one complete PQC handshake over a pipe. Key
// generation is excluded from the timer, so the number is the handshake itself.
func BenchmarkInboundHandshake(b *testing.B) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		alice, err := InitSession(logger, 26002, 26001)
		if err != nil {
			b.Fatal(err)
		}
		bob, err := InitSession(logger, 26004, 26003)
		if err != nil {
			b.Fatal(err)
		}
		alicePeer, err := kbc.ParsePeerKeys(map[string][]byte{
			"x25519": alice.OwnKeys.X25519Public.Bytes(),
			"mlkem":  alice.OwnKeys.MlKemPublic.Bytes(),
		})
		if err != nil {
			b.Fatal(err)
		}
		bob.PeerPubKeys = alicePeer
		alice.ExpectedPeerFingerprint, err = bob.OwnKeys.Fingerprint()
		if err != nil {
			b.Fatal(err)
		}
		bobEnd, aliceEnd := net.Pipe()
		errCh := make(chan error, 1)
		b.StartTimer()

		go func() { errCh <- PerformOutboundHandshakeOnConn(bob, bobEnd) }()
		if err := PerformInboundHandshake(alice, aliceEnd); err != nil {
			b.Fatal(err)
		}
		if err := <-errCh; err != nil {
			b.Fatal(err)
		}

		b.StopTimer()
		bobEnd.Close()
		aliceEnd.Close()
		b.StartTimer()
	}
}
