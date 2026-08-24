// ABOUTME: Regression tests pinning the nonce direction prefix invariant across the in-band
// ABOUTME: rekey ratchet: the prefix bytes must stay constant across epochs and counters.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

// A legitimate frame must still be accepted once the writer has ratcheted past epoch 0.
// Regression guard: the prefix check in secureconn.go Read must never reject a valid
// frame just because the rekey ratchet advanced the epoch.
func TestReaderAcceptsCorrectPrefixAfterRatchet(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := testkit.RandBytes(t, 32)

			var sink bytes.Buffer
			w := NewSecureWriterWithPrefix(&sink, key, suite, NoncePrefixOutbound)
			require.NoError(t, w.ratchet(nil), "ratchet the writer to epoch 1")
			require.Equal(t, uint16(1), w.epoch)

			_, err := w.Write([]byte("post-ratchet payload"))
			require.NoError(t, err)

			r := NewSecureReader(bytes.NewReader(sink.Bytes()), key, suite, NoncePrefixOutbound)
			r.keyUpdate.Store(true)
			pt, err := r.Read()
			require.NoError(t, err, "a correctly-prefixed frame must open after the writer ratcheted")
			require.Equal(t, []byte("post-ratchet payload"), pt)
		})
	}
}

// Many frames in sequence, crossing several epoch bumps, must all decrypt: the counter reset
// at each bump must not disturb the prefix bytes, and the reader must follow every bump in turn.
func TestReaderAcceptsManyFramesAcrossEpochBump(t *testing.T) {
	shrinkRekeyThresholds(t, 2048, 1<<20) // trigger on bytes, matching TestSecureConn_WriterRatchetsEpochOnWire

	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := testkit.RandBytes(t, 32)
			sink := &writeSink{}
			sc := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
			sc.SetKeyUpdate(true)

			const msgSize = 1024
			const numMsgs = 8
			payloads := make([][]byte, numMsgs)
			for i := range payloads {
				payloads[i] = testkit.RandBytes(t, msgSize)
				_, err := sc.Write(payloads[i])
				require.NoError(t, err)
			}
			require.Greater(t, sc.w.epoch, uint16(0), "the writer must have ratcheted at least once")

			frames := nonces(t, sink.buf.Bytes())
			require.Len(t, frames, numMsgs)
			for i, n := range frames {
				require.Equal(t, prefixBytes(NoncePrefixOutbound), n[:4],
					"frame %d must keep the OUTB prefix regardless of epoch/counter", i)
			}

			r := NewSecureReader(bytes.NewReader(sink.buf.Bytes()), key, suite, NoncePrefixOutbound)
			r.keyUpdate.Store(true)
			for i, want := range payloads {
				got, err := r.Read()
				require.NoError(t, err, "frame %d must decrypt", i)
				require.Equal(t, want, got, "frame %d payload mismatch", i)
			}
		})
	}
}

// A reflected frame (written by the OUTBOUND writer, fed to a reader expecting the opposite
// direction) must still be rejected once the writer has ratcheted past epoch 0, not just at
// epoch 0: the prefix check runs before any epoch-aware decryption, so a later epoch must not
// open a bypass window.
func TestReaderRejectsWrongDirectionPrefixAfterRatchet(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := testkit.RandBytes(t, 32)

			var sink bytes.Buffer
			w := NewSecureWriterWithPrefix(&sink, key, suite, NoncePrefixOutbound)
			require.NoError(t, w.ratchet(nil), "ratchet the writer to epoch 1")
			require.Equal(t, uint16(1), w.epoch)

			_, err := w.Write([]byte("control-frame-ratcheted"))
			require.NoError(t, err)
			frame := sink.Bytes()

			// Positive control: the matching-direction reader still opens it post-ratchet.
			rOK := NewSecureReader(bytes.NewReader(frame), key, suite, NoncePrefixOutbound)
			rOK.keyUpdate.Store(true)
			_, err = rOK.Read()
			require.NoError(t, err, "correctly-directed frame must open after ratchet")

			// The reflection: a reader on the same key but the OPPOSITE direction, same epoch.
			rReflect := NewSecureReader(bytes.NewReader(frame), key, suite, NoncePrefixInbound)
			rReflect.keyUpdate.Store(true)
			_, err = rReflect.Read()
			require.Error(t, err, "reader accepted a reflected frame carrying the wrong direction prefix at epoch > 0")
		})
	}
}
