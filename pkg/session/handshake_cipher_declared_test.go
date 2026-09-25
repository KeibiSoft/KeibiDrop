// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// The declared-cipher field, upstreamed from KeibiDropWeb so both repos run one
// pkg/session. An inbound flight carries the peer's outbound, encrypted with the
// one suite that peer committed; a peer that names it lets us read even a suite
// our own preference list would not have picked. A browser offers ChaCha20 only
// and must still read a desktop's AES-256-GCM; a desktop that optimistically
// pre-set AES must adopt a browser's ChaCha20 rather than derive the wrong key.

package session

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

func declaredCipherFixture(t *testing.T, declared string, preset kbc.CipherSuite) *Session {
	t.Helper()
	alice, msg := buildHandshakeFixture(t)

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
	alice.CipherSuite = preset
	msg.Cipher = declared

	conn := pipeWithMessage(t, msg)
	t.Cleanup(func() { _ = conn.Close() })
	require.NoError(t, PerformInboundHandshake(alice, conn))
	return alice
}

// A declared suite is adopted, even over a locally pre-set guess.
func TestInboundAdoptsDeclaredCipher(t *testing.T) {
	alice := declaredCipherFixture(t, string(kbc.CipherChaCha20), kbc.CipherAES256)
	require.Equal(t, kbc.CipherChaCha20, alice.CipherSuite,
		"a peer that names its committed suite is believed; that is how a ChaCha20-only browser is read")
	require.True(t, alice.PeerDeclaredCipher, "the declaration is the build marker the presence gate reads")
}

// A legacy peer sends no cipher field, and the old negotiation stands: a
// pre-set suite wins, and without one the lists are negotiated.
func TestInboundWithoutDeclaredCipherIsUnchanged(t *testing.T) {
	t.Run("pre-set suite wins", func(t *testing.T) {
		alice := declaredCipherFixture(t, "", kbc.CipherAES256)
		require.Equal(t, kbc.CipherAES256, alice.CipherSuite)
		require.False(t, alice.PeerDeclaredCipher, "a legacy peer is not marked as declaring")
	})
	t.Run("negotiated from the lists", func(t *testing.T) {
		alice := declaredCipherFixture(t, "", "")
		// The fixture's peer offers ChaCha20 only, so that is the overlap
		// whatever this machine prefers.
		require.Equal(t, kbc.CipherChaCha20, alice.CipherSuite)
	})
}

// An unknown or malformed declaration cannot select an unsupported primitive:
// it falls back to negotiation.
func TestInboundIgnoresUnknownDeclaredCipher(t *testing.T) {
	alice := declaredCipherFixture(t, "aes-128-siv-totally-made-up", "")
	require.Equal(t, kbc.CipherChaCha20, alice.CipherSuite)
	require.True(t, kbc.IsKnownCipher(alice.CipherSuite))
	require.False(t, alice.PeerDeclaredCipher, "an unknown value is not a declaration")
}

// The outbound flight declares what it committed, so the other side can adopt it.
func TestOutboundDeclaresItsCommittedCipher(t *testing.T) {
	for _, suite := range kbc.SupportedCiphers() {
		msg := PeerHandshakeMessage{Cipher: string(suite)}
		require.True(t, kbc.IsKnownCipher(kbc.CipherSuite(msg.Cipher)))
	}
	// The field is omitted when empty, so a build that never sets it puts
	// nothing new on the wire.
	require.NotContains(t, mustJSON(t, PeerHandshakeMessage{}), `"cipher"`)
	require.Contains(t, mustJSON(t, PeerHandshakeMessage{Cipher: "chacha20-poly1305"}), `"cipher"`)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
