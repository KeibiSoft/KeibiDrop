// ABOUTME: Tests for the inbound handshake flow, covering TOFU acceptance
// ABOUTME: and relay-mode fingerprint rejection.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package session

import (
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"testing"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// buildHandshakeFixture creates two sessions (alice = inbound, bob = outbound)
// and returns bob's handshake message ready to be sent over a pipe.
func buildHandshakeFixture(t *testing.T) (*Session, PeerHandshakeMessage) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Alice (inbound side)
	alice, err := InitSession(logger, 26002, 26001)
	require.NoError(t, err, "InitSession for alice")

	// Bob (outbound side) — we only need his keys to build the message
	bob, err := InitSession(logger, 26004, 26003)
	require.NoError(t, err, "InitSession for bob")

	// Bob encapsulates seeds using Alice's public keys
	seed1 := kbc.GenerateSeed()
	encSeed1, err := kbc.X25519Encapsulate(seed1, bob.OwnKeys.X25519Private, alice.OwnKeys.X25519Public)
	require.NoError(t, err, "X25519Encapsulate")

	_, encSeed2 := alice.OwnKeys.MlKemPublic.Encapsulate()

	msg := PeerHandshakeMessage{
		Fingerprint: bob.OwnFingerprint,
		PublicKeys: map[string]string{
			"x25519": encodeBase64(bob.OwnKeys.X25519Public.Bytes()),
			"mlkem":  encodeBase64(bob.OwnKeys.MlKemPublic.Bytes()),
		},
		EncSeeds: map[string]string{
			"x25519": encodeBase64(encSeed1),
			"mlkem":  encodeBase64(encSeed2),
		},
		OutboundPort:     26004,
		SupportedCiphers: []string{"chacha20-poly1305"},
	}

	return alice, msg
}

// pipeWithMessage creates a net.Pipe, writes the length-prefixed JSON message
// to one end, and returns the other end for the handshake to read from.
// Format matches PerformInboundHandshake: 4-byte big-endian length + JSON body.
func pipeWithMessage(t *testing.T, msg PeerHandshakeMessage) net.Conn {
	t.Helper()
	server, client := net.Pipe()

	go func() {
		defer server.Close()
		payload, err := json.Marshal(msg)
		if err != nil {
			return
		}
		var lenBuf [4]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(payload))) //nolint:gosec // G115: test payload is small
		_, _ = server.Write(lenBuf[:])
		_, _ = server.Write(payload)
	}()

	return client
}

func TestPerformInboundHandshake_TOFU_AcceptsAnyFingerprint(t *testing.T) {
	alice, msg := buildHandshakeFixture(t)
	alice.ExpectedPeerFingerprint = "TOFU"

	conn := pipeWithMessage(t, msg)
	defer conn.Close()

	err := PerformInboundHandshake(alice, conn)
	require.NoError(t, err, "TOFU handshake should succeed with any valid peer")
}

func TestPerformInboundHandshake_TOFU_StoresActualFingerprint(t *testing.T) {
	alice, msg := buildHandshakeFixture(t)
	alice.ExpectedPeerFingerprint = "TOFU"

	conn := pipeWithMessage(t, msg)
	defer conn.Close()

	err := PerformInboundHandshake(alice, conn)
	require.NoError(t, err)

	require.NotEqual(t, "TOFU", alice.ExpectedPeerFingerprint,
		"After TOFU handshake, ExpectedPeerFingerprint must be replaced with the actual fingerprint")
	require.NotEmpty(t, alice.ExpectedPeerFingerprint,
		"After TOFU handshake, ExpectedPeerFingerprint must not be empty")
}

func TestPerformInboundHandshake_RelayMode_RejectsMismatch(t *testing.T) {
	alice, msg := buildHandshakeFixture(t)
	alice.ExpectedPeerFingerprint = "obviously-wrong-fingerprint"

	conn := pipeWithMessage(t, msg)
	defer conn.Close()

	err := PerformInboundHandshake(alice, conn)
	require.Error(t, err, "Handshake must fail when fingerprint does not match")
	require.Contains(t, err.Error(), "fingerprint mismatch")
}

func TestPerformInboundHandshake_ExactMatch_Succeeds(t *testing.T) {
	alice, msg := buildHandshakeFixture(t)

	// Compute the actual fingerprint of bob's keys so alice expects it
	bobPubKeys := make(map[string][]byte, 2)
	for k, v := range msg.PublicKeys {
		decoded, err := decodeBase64(v)
		require.NoError(t, err)
		bobPubKeys[k] = decoded
	}
	peerKeys, err := kbc.ParsePeerKeys(bobPubKeys)
	require.NoError(t, err)
	fp, err := peerKeys.Fingerprint()
	require.NoError(t, err)

	alice.ExpectedPeerFingerprint = fp

	conn := pipeWithMessage(t, msg)
	defer conn.Close()

	err = PerformInboundHandshake(alice, conn)
	require.NoError(t, err, "Handshake must succeed when fingerprint matches exactly")
}
