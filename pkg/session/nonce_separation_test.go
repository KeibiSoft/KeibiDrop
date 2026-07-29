// ABOUTME: Regression tests for the AEAD nonce-reuse fix: the two endpoints of a socket must use opposite direction nonce prefixes.
// ABOUTME: Guards the handshake wiring (inbound=INBD, outbound=OUTB), the round-trip, 0.3.x wire-compat, and prefix persistence across rekey.

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
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// writeSink is a net.Conn whose writes accumulate in a buffer so a test can inspect the frames.
type writeSink struct{ buf bytes.Buffer }

func (s *writeSink) Write(p []byte) (int, error)     { return s.buf.Write(p) }
func (s *writeSink) Read([]byte) (int, error)        { return 0, io.EOF }
func (s *writeSink) Close() error                    { return nil }
func (s *writeSink) LocalAddr() net.Addr             { return sinkAddr{} }
func (s *writeSink) RemoteAddr() net.Addr            { return sinkAddr{} }
func (s *writeSink) SetDeadline(time.Time) error     { return nil }
func (s *writeSink) SetReadDeadline(time.Time) error { return nil }
func (s *writeSink) SetWriteDeadline(time.Time) error {
	return nil
}

type sinkAddr struct{}

func (sinkAddr) Network() string { return "sink" }
func (sinkAddr) String() string  { return "sink" }

func prefixBytes(p uint32) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, p)
	return b
}

// nonces parses the nonce of each frame in a captured stream ([4-byte len][nonce]...).
func nonces(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) >= lengthHeaderSize {
		n := binary.BigEndian.Uint32(b[:lengthHeaderSize])
		b = b[lengthHeaderSize:]
		require.GreaterOrEqual(t, uint32(len(b)), n)
		out = append(out, append([]byte(nil), b[:kbc.NonceSize]...))
		b = b[n:]
	}
	return out
}

func firstNonce(t *testing.T, key []byte, suite kbc.CipherSuite, prefix uint32) []byte {
	t.Helper()
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, suite, prefix)
	require.NoError(t, sc.WriteMessage([]byte("first")))
	got := nonces(t, sink.buf.Bytes())
	require.Len(t, got, 1)
	return got[0]
}

// Core guard: on a shared key, the outbound and inbound endpoints seal their first message under
// different nonces (OUTB|1 vs INBD|1), so no two-time pad exists.
func TestNonceDirectionSeparation(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			out := firstNonce(t, key, suite, NoncePrefixOutbound)
			in := firstNonce(t, key, suite, NoncePrefixInbound)
			require.NotEqual(t, out, in, "the two endpoints of a socket must not share a nonce")
			require.Equal(t, prefixBytes(NoncePrefixOutbound), out[:4])
			require.Equal(t, prefixBytes(NoncePrefixInbound), in[:4])
			require.Equal(t, uint64(1), binary.BigEndian.Uint64(out[4:]))
			require.Equal(t, uint64(1), binary.BigEndian.Uint64(in[4:]))
		})
	}
}

// Guards the handshake wiring: after a real handshake the dialing side's Outbound writer uses OUTB
// and the accepting side's Inbound writer uses INBD, though both ends share one key.
func TestHandshakeAssignsOppositeNoncePrefixes(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	alice, err := InitSession(log, config.OutboundPort, config.InboundPort)
	require.NoError(t, err)
	bob, err := InitSession(log, config.OutboundPort, config.InboundPort)
	require.NoError(t, err)

	alice.PeerPubKeys = &kbc.PeerKeys{X25519Public: bob.OwnKeys.X25519Public, MlKemPublic: bob.OwnKeys.MlKemPublic}
	alice.ExpectedPeerFingerprint = bob.OwnFingerprint
	bob.ExpectedPeerFingerprint = alice.OwnFingerprint

	ac, bc := net.Pipe()
	t.Cleanup(func() { ac.Close(); bc.Close() })
	errCh := make(chan error, 1)
	go func() { errCh <- PerformInboundHandshake(bob, bc) }()
	require.NoError(t, PerformOutboundHandshakeOnConn(alice, ac))
	require.NoError(t, <-errCh)

	require.Equal(t, NoncePrefixOutbound, alice.Session.Outbound.writerPrefix, "dialing side must use the outbound prefix")
	require.Equal(t, NoncePrefixInbound, bob.Session.Inbound.writerPrefix, "accepting side must use the inbound prefix")
	require.Equal(t, alice.SEKOutbound, bob.SEKInbound, "sanity: the socket's two ends share one key")
}

// The direction-separated conns still interoperate: each side decrypts the other's frames.
func TestRoundTripBothDirections(t *testing.T) {
	for _, suite := range cipherSuites {
		suite := suite
		t.Run(string(suite), func(t *testing.T) {
			key := randomKey(t)
			c1, c2 := net.Pipe()
			t.Cleanup(func() { c1.Close(); c2.Close() })
			outbound := NewSecureConn(c1, key, suite, NoncePrefixOutbound)
			inbound := NewSecureConn(c2, key, suite, NoncePrefixInbound)

			for _, dir := range []struct {
				name     string
				from, to *SecureConn
			}{
				{"outbound_to_inbound", outbound, inbound},
				{"inbound_to_outbound", inbound, outbound},
			} {
				t.Run(dir.name, func(t *testing.T) {
					msg := randomBytes(t, 300)
					errCh := make(chan error, 1)
					go func() { _, e := dir.from.Write(msg); errCh <- e }()
					got := make([]byte, len(msg))
					_, err := io.ReadFull(dir.to, got)
					require.NoError(t, err)
					require.Equal(t, msg, got)
					require.NoError(t, <-errCh)
				})
			}
		})
	}
}

// 0.3.x wire-compat guard: a prefix-agnostic reader (nonce taken from the frame) decrypts an
// INBD-sealed frame; the fix does not change the wire format.
func TestReaderDecryptsInboundPrefixFrame(t *testing.T) {
	key := randomKey(t)
	suite := kbc.CipherAES256
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, suite, NoncePrefixInbound)
	payload := []byte("wire-compat payload")
	require.NoError(t, sc.WriteMessage(payload))

	rdr := NewSecureReader(bytes.NewReader(sink.buf.Bytes()), key, suite, NoncePrefixInbound)
	got, err := rdr.Read()
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

// A rekey must keep the direction prefix: UpdateKey must not revert an inbound writer to outbound.
func TestUpdateKeyPersistsWriterPrefix(t *testing.T) {
	key := randomKey(t)
	newKey := randomKey(t)
	suite := kbc.CipherAES256
	sink := &writeSink{}
	sc := NewSecureConn(sink, key, suite, NoncePrefixInbound)
	require.NoError(t, sc.WriteMessage([]byte("before")))
	sc.UpdateKey(newKey)
	require.NoError(t, sc.WriteMessage([]byte("after")))

	got := nonces(t, sink.buf.Bytes())
	require.Len(t, got, 2)
	require.Equal(t, prefixBytes(NoncePrefixInbound), got[0][:4])
	require.Equal(t, prefixBytes(NoncePrefixInbound), got[1][:4], "UpdateKey must keep the inbound prefix")
	require.Equal(t, uint64(1), binary.BigEndian.Uint64(got[1][4:]), "counter resets after rekey")
}
