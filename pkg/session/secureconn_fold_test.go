// ABOUTME: Tests for the SecureConn entropy fold: the writer folds a staged KEM secret,
// ABOUTME: the reader self-synchronizes plain-or-folded, and the responder writer is gated.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"io"
	"sync"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// keyUpdateReader builds a receive-ratchet reader over raw bytes, optionally with a
// staged fold secret, mirroring how the receive side is configured on a live conn.
func keyUpdateReader(raw, key []byte, suite kbc.CipherSuite, staged []byte) *SecureReader {
	r := NewSecureReader(bytes.NewReader(raw), key, suite, NoncePrefixOutbound)
	r.keyUpdate.Store(true)
	if staged != nil {
		r.stageFold(staged)
	}
	return r
}

// An initiator writer folds the staged KEM secret on its next write (prompt, no threshold
// wait); a reader holding the same secret follows and commits the folded epoch, while a
// reader without it cannot open the frame, proving the epoch was genuinely folded.
func TestSecureConn_InitiatorWriterFoldsAndReaderFollows(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sink := &writeSink{}
	w := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
	w.SetKeyUpdate(true)

	S := randomBytes(t, kbc.KeySize)
	w.StageEntropyFold(S, true) // initiator: the next write folds
	_, err := w.Write([]byte("folded payload"))
	require.NoError(t, err)

	require.Equal(t, uint16(1), w.w.epoch, "the writer folded to epoch 1")
	require.Nil(t, w.stagedWriterFold, "the staged fold is single-use")

	raw := sink.buf.Bytes()

	rOK := keyUpdateReader(raw, key, suite, S)
	pt, err := rOK.Read()
	require.NoError(t, err)
	require.Equal(t, []byte("folded payload"), pt)
	require.True(t, rOK.foldCommitted.Load(), "committing a folded epoch raises the peer-folded flag")

	rNo := keyUpdateReader(raw, key, suite, nil)
	_, err = rNo.Read()
	require.Error(t, err, "a folded frame must not open under the plain ratchet")
}

// A plain ratchet must still open while a fold is staged: the reader tries the folded
// candidate first, and when it fails must fall through to plain with the ciphertext buffer
// intact. This is the regression guard for opening each candidate into a fresh buffer
// rather than ciphertext[:0], which AEAD Open zeroes on failure.
func TestSecureConn_ReaderPlainFallthroughWhileFoldStaged(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sink := &writeSink{}
	w := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
	w.SetKeyUpdate(true)

	require.NoError(t, w.w.ratchet(nil)) // a PLAIN bump to epoch 1
	require.NoError(t, w.WriteMessage([]byte("plain frame")))

	r := keyUpdateReader(sink.buf.Bytes(), key, suite, randomBytes(t, kbc.KeySize))
	pt, err := r.Read()
	require.NoError(t, err, "a plain frame opens while a fold is staged (folded tried first, then plain)")
	require.Equal(t, []byte("plain frame"), pt)
	require.False(t, r.foldCommitted.Load(), "a plain commit does not consume the staged fold")
	require.NotNil(t, r.stagedFold, "the fold stays staged until an actual folded frame arrives")
}

// A responder writer must not fold until its own reader has committed a folded frame from
// the initiator. With the gate closed and no threshold pressure, a staged responder does
// not ratchet at all; once the gate opens it folds on the next write.
func TestSecureConn_ResponderWriterGatedUntilPeerFolds(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sink := &writeSink{}
	resp := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
	resp.SetKeyUpdate(true)

	S := randomBytes(t, kbc.KeySize)
	resp.StageEntropyFold(S, false) // responder: gated

	_, err := resp.Write([]byte("gated"))
	require.NoError(t, err)
	require.Equal(t, uint16(0), resp.w.epoch, "a gated responder does not fold on its own")
	require.NotNil(t, resp.stagedWriterFold, "the fold stays staged while gated")

	resp.r.foldCommitted.Store(true) // its reader saw the initiator's fold
	_, err = resp.Write([]byte("healed"))
	require.NoError(t, err)
	require.Equal(t, uint16(1), resp.w.epoch, "once ungated the responder folds on the next write")
	require.Nil(t, resp.stagedWriterFold, "the fold is consumed after it applies")
}

// The full round-trip over a live pair: an initiator folds mid-stream, the reader follows
// across the folded epoch with the conn never closed, and the data is intact.
func TestSecureConn_FoldRoundTripAcrossPair(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			writer, reader := newConnPair(t, key, suite)
			writer.SetKeyUpdate(true)
			reader.SetKeyUpdate(true)

			S := randomBytes(t, kbc.KeySize)
			writer.StageEntropyFold(S, true)
			reader.StageEntropyFold(S, false)

			const msgs, sz = 20, 512
			total := msgs * sz
			original := randomBytes(t, total)
			errCh := make(chan error, 1)
			go func() {
				var werr error
				for i := 0; i < msgs; i++ {
					if _, e := writer.Write(original[i*sz : (i+1)*sz]); e != nil {
						werr = e
						break
					}
				}
				errCh <- werr
			}()

			got := make([]byte, total)
			_, err := io.ReadFull(reader, got)
			require.NoError(t, err)
			require.Equal(t, original, got, "all data round-trips across the fold")
			require.NoError(t, <-errCh)

			require.Greater(t, writer.w.epoch, uint16(0), "the writer folded mid-stream")
			require.True(t, reader.r.foldCommitted.Load(), "the reader committed the folded epoch")
			require.False(t, writer.done.Load(), "the conn never closed during the fold")
			require.False(t, reader.done.Load())
		})
	}
}

// The adversarial ordering case the security review called out: under active load the
// responder crosses its ratchet threshold BEFORE the initiator has S. A gated responder must
// then plain-ratchet across several epochs (never folding, never tearing down) with its reader
// following every plain bump; once its reader commits the initiator's fold the gate opens and
// the next write folds, and the reader follows across the folded epoch. Data is intact through
// both phases.
func TestSecureConn_GatedResponderPlainRatchetsUnderLoadThenFolds(t *testing.T) {
	shrinkRekeyThresholds(t, 4096, 5) // a burst forces repeated plain ratchets while gated
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)
	writer.SetKeyUpdate(true)
	reader.SetKeyUpdate(true)

	S := randomBytes(t, kbc.KeySize)
	writer.StageEntropyFold(S, false) // gated responder
	reader.StageEntropyFold(S, false) // its counterpart reader holds S to follow an eventual fold

	// Phase 1: the gated responder plain-ratchets under load. It must cross several epochs
	// without folding or tearing down, and the reader must follow every plain bump.
	const burst, sz = 40, 512
	payload := randomBytes(t, burst*sz)
	errCh := make(chan error, 1)
	go func() {
		var werr error
		for i := 0; i < burst; i++ {
			if _, e := writer.Write(payload[i*sz : (i+1)*sz]); e != nil {
				werr = e
				break
			}
		}
		errCh <- werr
	}()
	got := make([]byte, burst*sz)
	_, err := io.ReadFull(reader, got)
	require.NoError(t, err)
	require.NoError(t, <-errCh)
	require.Equal(t, payload, got, "data is intact while the gated responder plain-ratchets")
	require.Greater(t, writer.w.epoch, uint16(1), "the responder crossed its ratchet threshold several times under load")
	require.NotNil(t, writer.stagedWriterFold, "but it stayed gated: the fold is still pending")
	require.False(t, reader.r.foldCommitted.Load(), "no folded epoch was committed while gated")
	require.False(t, writer.done.Load(), "the conn never tore down under the ordering pressure")
	require.False(t, reader.done.Load())

	// Phase 2: the responder's reader commits the initiator's fold; the gate opens and the
	// next write folds, and the reader follows across the folded epoch.
	epochBeforeFold := writer.w.epoch
	writer.r.foldCommitted.Store(true) // its reader saw the initiator's fold
	go func() { _, e := writer.Write([]byte("post-fold payload")); errCh <- e }()
	post := make([]byte, len("post-fold payload"))
	_, err = io.ReadFull(reader, post)
	require.NoError(t, err)
	require.NoError(t, <-errCh)
	require.Equal(t, []byte("post-fold payload"), post, "data is intact across the fold")
	require.Greater(t, writer.w.epoch, epochBeforeFold, "the ungated responder folded on its next write")
	require.Nil(t, writer.stagedWriterFold, "the fold is consumed after it applies")
	require.True(t, reader.r.foldCommitted.Load(), "the reader committed the folded epoch")
}

// Staging must be race-free against concurrent writes and reads: a duplicate Rekey delivery
// re-staging the same secret cannot tear the writer/reader fold state. Staged as a gated
// responder so the writer only plain-ratchets (never folds into the data path), which keeps
// every frame decryptable while still exercising, under -race, the writer fold fields (wMu),
// the reader staged fold (foldMu, via the plain fallthrough at each epoch bump), and the
// foldCommitted flag.
func TestSecureConn_ConcurrentStageFoldNoRace(t *testing.T) {
	shrinkRekeyThresholds(t, 4096, 256) // force periodic plain ratchets so the reader hits epoch bumps
	key := randomKey(t)
	writer, reader := newConnPair(t, key, kbc.CipherChaCha20)
	writer.SetKeyUpdate(true)
	reader.SetKeyUpdate(true)
	S := randomBytes(t, kbc.KeySize)

	const iters = 300
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			writer.StageEntropyFold(S, false) // gated responder: never folds, but re-stages
			reader.StageEntropyFold(S, false)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := writer.Write(make([]byte, 64)); err != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 64)
		for i := 0; i < iters; i++ {
			if _, err := reader.Read(buf); err != nil {
				return
			}
		}
	}()
	wg.Wait()

	require.False(t, reader.r.foldCommitted.Load(),
		"a gated responder never folds, so no folded epoch is ever committed")
}
