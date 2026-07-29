// ABOUTME: Tests for in-band key-update capability negotiation: the handshake advertises and
// ABOUTME: learns the flag, and ApplyKeyUpdateNegotiation arms the ratchet only when both agree.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// A real handshake propagates key-update: the inbound side learns the dialer advertised it, arming the ratchet.
func TestInboundHandshakeLearnsKeyUpdateCapability(t *testing.T) {
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

	require.True(t, bob.PeerSupportsKeyUpdate, "inbound side must learn the dialer advertised key-update")
	require.True(t, bob.UseKeyUpdate(), "both new: ratchet enabled")

	// The capability is bound into the SEK; untampered, both ends still agree on the key.
	require.Equal(t, alice.SEKOutbound, bob.SEKInbound,
		"both ends derive the same key when the capability is untampered")
}

// The key-update capability is bound into the SEK: a relay stripping the plaintext bit makes the
// two ends derive different keys (fail-closed), not a silent downgrade to the non-FS fallback.
func TestKeyUpdateCapabilityBoundIntoSEK(t *testing.T) {
	seed1 := randomBytes(t, kbc.KeySize)
	seed2 := randomBytes(t, kbc.KeySize)

	// Dialer advertised key-update=true and binds its own capability.
	dialerOut, err := kbc.DeriveKey(kbc.CipherChaCha20, seed1, seed2, keyUpdateBinding(true))
	require.NoError(t, err)

	// Untampered: the inbound side received true and agrees on the key.
	inboundOK, err := kbc.DeriveKey(kbc.CipherChaCha20, seed1, seed2, keyUpdateBinding(true))
	require.NoError(t, err)
	require.Equal(t, dialerOut, inboundOK, "an untampered handshake agrees on the key")

	// A relay stripped the bit, so the inbound side binds false and derives a different key.
	inboundStripped, err := kbc.DeriveKey(kbc.CipherChaCha20, seed1, seed2, keyUpdateBinding(false))
	require.NoError(t, err)
	require.NotEqual(t, dialerOut, inboundStripped,
		"a stripped key-update bit must break the shared key (fail-closed), not silently downgrade")
}

// New-to-old fails closed by design: the new peer always mixes the key-update capability binding
// into its SEK, diverging from an old peer's secrets-only key. No silent non-FS downgrade exists.
func TestKeyUpdateBindingBreaksOldPeerInterop(t *testing.T) {
	seed1 := randomBytes(t, kbc.KeySize)
	seed2 := randomBytes(t, kbc.KeySize)

	// What a pre-patch (old) peer computes: no capability binding, just the two secrets.
	oldSEK, err := kbc.DeriveKey(kbc.CipherChaCha20, seed1, seed2)
	require.NoError(t, err)

	// What a new peer computes over the identical secrets: the binding is always mixed in.
	newSEK, err := kbc.DeriveKey(kbc.CipherChaCha20, seed1, seed2, keyUpdateBinding(false))
	require.NoError(t, err)

	require.NotEqual(t, oldSEK, newSEK,
		"new-to-old must fail closed: the capability binding diverges the new peer's key from an "+
			"old peer's, so there is no silent downgrade")
}

// A key-update session must force a re-handshake as its ratchet epoch nears the wrap guard, where
// the ratchet stops advancing; otherwise the epoch pins and forward secrecy stalls.
func TestSession_NearEpochWrap(t *testing.T) {
	key := randomKey(t)
	newSess := func(peerKeyUpdate bool) *Session {
		return &Session{
			PeerSupportsKeyUpdate: peerKeyUpdate,
			Session: &SessionSockets{
				Inbound:  NewSecureConn(&writeSink{}, key, kbc.CipherChaCha20, NoncePrefixInbound),
				Outbound: NewSecureConn(&writeSink{}, key, kbc.CipherChaCha20, NoncePrefixOutbound),
			},
		}
	}

	s := newSess(true)
	require.False(t, s.NearEpochWrap(), "a fresh key-update session is far from the wrap guard")

	s.Session.Outbound.SetWriterEpochForTest(epochRehandshakeThreshold)
	require.True(t, s.NearEpochWrap(), "at the threshold a key-update session must re-handshake")

	// An old peer never ratchets, so it is never near-wrap; it rotates via re-handshake instead.
	old := newSess(false)
	old.Session.Outbound.SetWriterEpochForTest(epochRehandshakeThreshold)
	require.False(t, old.NearEpochWrap(), "an old peer rotates via re-handshake, not the ratchet")
}

// The monitor must default to RekeyEnabled so onReconnected rebuilds never drop the near-wrap
// re-handshake for a key-update pair.
func TestNewHealthMonitor_RekeyEnabledByDefault(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewHealthMonitor(&Session{}, nil, log)
	require.True(t, m.RekeyEnabled, "the monitor must default to rekey-enabled")
}

// An old peer's JSON has no key_update field; it must decode to false so negotiation defaults to
// the safe re-handshake fallback, not a one-sided ratchet.
func TestHandshakeDecodeDefaultsKeyUpdateOff(t *testing.T) {
	oldMsg := []byte(`{"fingerprint":"x","public_keys":{},"enc_seeds":{},"port":1,"supported_ciphers":["chacha20-poly1305"]}`)
	var msg PeerHandshakeMessage
	require.NoError(t, json.Unmarshal(oldMsg, &msg))
	require.False(t, msg.KeyUpdate, "missing key_update must default to false")
}

// UseKeyUpdate is the AND of both peers' capability; own is always true in this build.
func TestUseKeyUpdate(t *testing.T) {
	s := &Session{}
	require.False(t, s.UseKeyUpdate(), "peer not advertised -> off")
	s.PeerSupportsKeyUpdate = true
	require.True(t, s.UseKeyUpdate(), "both advertised -> on")
}

// ApplyKeyUpdateNegotiation arms or disarms the ratchet on both directions (reader and writer) to
// match the negotiated capability.
func TestApplyKeyUpdateNegotiationTogglesBothConns(t *testing.T) {
	key := randomKey(t)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	s := &Session{Session: &SessionSockets{
		Outbound: NewSecureConn(c1, key, kbc.CipherChaCha20, NoncePrefixOutbound),
		Inbound:  NewSecureConn(c2, key, kbc.CipherChaCha20, NoncePrefixInbound),
	}}

	// Peer did not advertise: the ratchet stays off on both.
	s.ApplyKeyUpdateNegotiation()
	require.False(t, s.Session.Outbound.useKeyUpdate.Load())
	require.False(t, s.Session.Inbound.useKeyUpdate.Load())

	// Peer advertised: both directions turn on, writer and reader alike.
	s.PeerSupportsKeyUpdate = true
	s.ApplyKeyUpdateNegotiation()
	require.True(t, s.Session.Outbound.useKeyUpdate.Load())
	require.True(t, s.Session.Inbound.useKeyUpdate.Load())
	require.True(t, s.Session.Outbound.r.keyUpdate.Load(), "outbound reader must be armed")
	require.True(t, s.Session.Inbound.r.keyUpdate.Load(), "inbound reader must be armed")
}

// End-to-end over a real PQC handshake: once both negotiate key-update, a mid-stream ratchet on the
// shared socket key decrypts continuously across the epoch bump.
func TestNegotiatedRatchetRoundTripsOverRealHandshake(t *testing.T) {
	shrinkRekeyThresholds(t, 4096, 1<<20)

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

	// Alice's outbound and Bob's inbound are the two ends of one link on a shared key; arm both
	// exactly as ApplyKeyUpdateNegotiation would.
	require.True(t, bob.UseKeyUpdate())
	writer, reader := alice.Session.Outbound, bob.Session.Inbound
	writer.SetKeyUpdate(true)
	reader.SetKeyUpdate(true)

	const msgs, sz = 50, 1024
	original := randomBytes(t, msgs*sz)
	werr := make(chan error, 1)
	go func() {
		var e error
		for i := 0; i < msgs; i++ {
			if _, we := writer.Write(original[i*sz : (i+1)*sz]); we != nil {
				e = we
				break
			}
		}
		werr <- e
	}()

	got := make([]byte, msgs*sz)
	_, err = io.ReadFull(reader, got)
	require.NoError(t, err)
	require.Equal(t, original, got, "data round-trips across the negotiated ratchet")
	require.NoError(t, <-werr)
	require.Greater(t, writer.w.epoch, uint16(0), "a ratchet fired mid-stream")
	require.Equal(t, writer.w.epoch, reader.r.epoch, "reader followed the writer epoch")
}
