// ABOUTME: Tests for the in-band SecureConn ratchet: the writer bumps the key epoch
// ABOUTME: mid-stream without closing the conn, the reader follows, and gaps are rejected.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"encoding/binary"
	"io"
	"sync"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// syncSink is a concurrency-safe writeSink, so a concurrent-writers test races only on
// the SecureConn's own ratchet state, not on the capture buffer.
type syncSink struct {
	mu sync.Mutex
	writeSink
}

func (s *syncSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeSink.Write(p)
}

func (s *syncSink) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

// shrinkRekeyThresholds lowers the rotation thresholds for a test and restores them.
func shrinkRekeyThresholds(t *testing.T, bytesT, msgsT uint64) {
	t.Helper()
	ob, om := RekeyBytesThreshold, RekeyMsgsThreshold
	RekeyBytesThreshold, RekeyMsgsThreshold = bytesT, msgsT
	t.Cleanup(func() { RekeyBytesThreshold, RekeyMsgsThreshold = ob, om })
}

func nonceEpoch(n []byte) uint16   { return binary.BigEndian.Uint16(n[4:6]) }
func nonceCounter(n []byte) uint64 { return binary.BigEndian.Uint64(n[4:]) & (1<<48 - 1) }

// The writer bumps the on-wire epoch once its own byte volume crosses the threshold,
// resets the counter under the new epoch, and never closes the conn to do it.
func TestSecureConn_WriterRatchetsEpochOnWire(t *testing.T) {
	shrinkRekeyThresholds(t, 2048, 1<<20) // trigger on bytes, not on message count

	key := randomKey(t)
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetKeyUpdate(true)

	const msgSize = 1024
	for i := 0; i < 6; i++ {
		_, err := sc.Write(make([]byte, msgSize))
		require.NoError(t, err)
	}

	frames := nonces(t, sink.buf.Bytes())
	require.Len(t, frames, 6)

	// 1024 bytes/write, threshold 2048: epoch bumps every two writes.
	require.Equal(t, []uint16{0, 0, 1, 1, 2, 2}, []uint16{
		nonceEpoch(frames[0]), nonceEpoch(frames[1]), nonceEpoch(frames[2]),
		nonceEpoch(frames[3]), nonceEpoch(frames[4]), nonceEpoch(frames[5]),
	}, "epoch advances after each threshold crossing")
	// The counter resets to 1 at every epoch bump.
	require.Equal(t, uint64(1), nonceCounter(frames[2]), "counter resets on the first bump")
	require.Equal(t, uint64(2), nonceCounter(frames[3]))
	require.Equal(t, uint64(1), nonceCounter(frames[4]), "counter resets on the second bump")
	require.False(t, sc.done.Load(), "the ratchet must not close the conn")
}

// A connected pair with key-update on streams past the threshold: the reader must
// decrypt continuously across every epoch bump, with the conn never closed.
func TestSecureConn_RatchetRoundTripAcrossEpochs(t *testing.T) {
	shrinkRekeyThresholds(t, 4096, 1<<20)

	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			writer, reader := newConnPair(t, key, suite)
			writer.SetKeyUpdate(true)
			reader.SetKeyUpdate(true)

			const msgs, sz = 50, 1024
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
			require.Equal(t, original, got, "all data round-trips across the ratchet")
			require.NoError(t, <-errCh)

			require.Greater(t, writer.w.epoch, uint16(0), "writer ratcheted mid-stream")
			require.Equal(t, writer.w.epoch, reader.r.epoch, "reader followed the writer's epoch")
			require.False(t, writer.done.Load(), "conn never closed during the ratchet")
			require.False(t, reader.done.Load())
		})
	}
}

// A far-future epoch (a gap) must be rejected before the reader derives any key, so a forged frame
// cannot force an unbounded run of HKDF steps.
func TestSecureConn_ReaderRejectsEpochGap(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sink := &writeSink{}

	w := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
	w.SetKeyUpdate(true)
	// Drive the writer two epochs ahead, then seal a frame at epoch 2.
	require.NoError(t, w.w.ratchet(nil))
	require.NoError(t, w.w.ratchet(nil))
	require.NoError(t, w.WriteMessage([]byte("gap frame")))

	// A reader still at epoch 0 must reject the epoch-2 frame (0+1 is the max it accepts).
	r := NewSecureReader(bytes.NewReader(sink.buf.Bytes()), key, suite, NoncePrefixOutbound)
	r.keyUpdate.Store(true)
	_, err := r.Read()
	require.Error(t, err)
	require.Contains(t, err.Error(), "epoch")
}

// A replayed or rolled-back frame (nonce not strictly increasing) must be rejected.
func TestSecureConn_ReaderRejectsReplay(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherChaCha20
	sink := &writeSink{}

	w := NewSecureConn(sink, key, suite, NoncePrefixOutbound)
	w.SetKeyUpdate(true)
	require.NoError(t, w.WriteMessage([]byte("frame one")))
	require.NoError(t, w.WriteMessage([]byte("frame two")))

	raw := sink.buf.Bytes()
	frame1Len := lengthHeaderSize + int(binary.BigEndian.Uint32(raw[:lengthHeaderSize]))
	frame1 := raw[:frame1Len]

	// Stream: frame one, frame two, then frame one again (a replay of the first nonce).
	stream := append(append([]byte(nil), raw...), frame1...)
	r := NewSecureReader(bytes.NewReader(stream), key, suite, NoncePrefixOutbound)
	r.keyUpdate.Store(true)

	_, err := r.Read()
	require.NoError(t, err)
	_, err = r.Read()
	require.NoError(t, err)
	_, err = r.Read()
	require.Error(t, err, "a replayed (non-increasing) nonce must be rejected")
}

// The writer must stop ratcheting before the uint16 epoch wraps, so a long-lived conn never resets
// the counter under an already-passed epoch (which would brick the reader's monotonic gate).
func TestSecureConn_WriterStopsRatchetingAtEpochGuard(t *testing.T) {
	shrinkRekeyThresholds(t, 1, 1<<20) // ratchet on essentially every write

	key := randomKey(t)
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetKeyUpdate(true)

	// Fast-forward the writer to just below the guard.
	sc.w.epoch = epochWrapGuard - 1
	sc.w.nonce.SetEpoch(epochWrapGuard - 1)

	for i := 0; i < 20; i++ {
		_, err := sc.Write([]byte("xy"))
		require.NoError(t, err)
	}

	require.Equal(t, epochWrapGuard, sc.w.epoch, "writer holds at the guard, never wraps to 0")
	for _, n := range nonces(t, sink.buf.Bytes()) {
		require.GreaterOrEqual(t, nonceEpoch(n), epochWrapGuard-1, "no frame carries a wrapped epoch")
	}
}

// Concurrent writers must never reuse or reorder a nonce: wMu serializes the seal and the inline
// ratchet so the epoch bump is single-flight. Defence-in-depth (gRPC uses a single writer).
func TestSecureConn_ConcurrentWritersNoNonceReuse(t *testing.T) {
	shrinkRekeyThresholds(t, 4096, 256) // ratchet often, on both bytes and message count

	key := randomKey(t)
	sink := &syncSink{}
	sc := NewSecureConn(sink, key, kbc.CipherChaCha20, NoncePrefixOutbound)
	sc.SetKeyUpdate(true)

	const goroutines, per = 8, 200
	var wg sync.WaitGroup
	errCh := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if _, err := sc.Write(make([]byte, 64)); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	frames := nonces(t, sink.bytes())
	require.Len(t, frames, goroutines*per)

	seen := make(map[string]struct{}, len(frames))
	var prev uint64
	for i, n := range frames {
		require.NotContains(t, seen, string(n), "a nonce was reused across concurrent writers")
		seen[string(n)] = struct{}{}
		v := binary.BigEndian.Uint64(n[4:]) // epoch:counter, must be strictly increasing
		if i > 0 {
			require.Greater(t, v, prev, "nonces must be strictly increasing on the wire")
		}
		prev = v
	}
}
