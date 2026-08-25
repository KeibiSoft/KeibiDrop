// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests that the reader rejects a frame carrying the wrong nonce
// ABOUTME: direction prefix, closing a same-key reflection on one socket.

package session

import (
	"bytes"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// A frame carries its writer's direction prefix in the nonce. Reader and writer on
// one socket share a key, separated only by that prefix, so the reader MUST reject a
// frame whose prefix is not the peer writer's expected one. Otherwise an on-path
// attacker can reflect a frame the peer itself wrote back at the same key, and it
// opens cleanly.
func TestReaderRejectsWrongDirectionPrefix(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := testkit.RandBytes(t, 32)

			var sink bytes.Buffer
			w := NewSecureWriterWithPrefix(&sink, key, suite, NoncePrefixOutbound)
			if _, err := w.Write([]byte("control-frame")); err != nil {
				t.Fatal(err)
			}
			frame := sink.Bytes()

			// Positive control: a reader that expects the peer to write OUTBOUND opens it.
			rOK := NewSecureReader(bytes.NewReader(frame), key, suite, NoncePrefixOutbound)
			if _, err := rOK.Read(); err != nil {
				t.Fatalf("correctly-directed frame must open: %v", err)
			}

			// The reflection: a reader on the same key but the OPPOSITE direction is fed
			// the OUTBOUND frame. It must be rejected. Before the fix it opens cleanly.
			rReflect := NewSecureReader(bytes.NewReader(frame), key, suite, NoncePrefixInbound)
			if _, err := rReflect.Read(); err == nil {
				t.Fatal("reader accepted a frame carrying the wrong direction prefix (reflection)")
			}
		})
	}
}
