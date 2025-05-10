package session

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// PeerHandshakeMessage defines the JSON payload sent during handshake.
type PeerHandshakeMessage struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	EncSeeds    map[string]string `json:"enc_seeds"`   // optional for key encapsulation
}

// TODO: Check out-of-band fingerprints.
// TODO: Add proper uniform logger.Error messages/ logger.Info("Success")

// PerformUnsecureInboundHandshake handles the first plaintext connection from Bob to Alice.
func PerformUnsecureInboundHandshake(session *Session, conn net.Conn) error {
	logger := session.logger.New("phase", "inbound-handshake")

	// Read JSON
	var msg PeerHandshakeMessage
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		return fmt.Errorf("invalid handshake format: %w", err)
	}

	// Compute and compare fingerprint
	computed, err := ComputeFingerprintFromBase64Keys(msg.PublicKeys)
	if err != nil {
		return fmt.Errorf("fingerprint computation failed: %w", err)
	}
	if computed != session.ExpectedPeerFingerprint {
		return fmt.Errorf("fingerprint mismatch: got %s, expected %s", computed, session.ExpectedPeerFingerprint)
	}

	session.PeerFingerprint = msg.Fingerprint
	session.PeerPubKeys = make(map[string][]byte)
	for k, v := range msg.PublicKeys {
		decoded, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return fmt.Errorf("invalid base64 key for %s: %w", k, err)
		}
		session.PeerPubKeys[k] = decoded
	}

	// Wait for user to confirm out-of-band fingerprint
	logger.Info("Peer fingerprint verified, awaiting user confirmation")
	// In real UI, this would be blocking for user approval
	if err := session.Transition(SessionStateVerified); err != nil {
		return err
	}
	return nil
}

// PerformUnsecureOutboundHandshake connects Alice to Bob and sends her handshake.
func PerformUnsecureOutboundHandshake(session *Session, remoteAddr string, seeds map[string][]byte) (net.Conn, error) {
	logger := session.logger.New("phase", "outbound-handshake")
	conn, err := net.DialTimeout("tcp", remoteAddr, 15*time.Second)
	if err != nil {
		logger.Error("Failed to dial", "addr", remoteAddr, "error", err)
		return nil, fmt.Errorf("failed to connect to %s: %w", remoteAddr, err)
	}

	pubKeys := map[string]string{
		"x25519": base64.StdEncoding.EncodeToString(session.OwnKeys.X25519Public.Bytes()),
		"mlkem":  base64.StdEncoding.EncodeToString(session.OwnKeys.MlKemPublic.Bytes()),
	}
	encSeeds := make(map[string]string)
	for k, seed := range seeds {
		encSeeds[k] = base64.StdEncoding.EncodeToString(seed)
	}

	msg := PeerHandshakeMessage{
		Fingerprint: session.ExpectedPeerFingerprint,
		PublicKeys:  pubKeys,
		EncSeeds:    encSeeds,
	}

	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to send handshake: %w", err)
	}

	return conn, nil
}

// UpgradeToSecureConn wraps an existing net.Conn after KEK is negotiated.
func UpgradeToSecureConn(conn net.Conn, kek []byte) *SecureConn {
	/* After KEK Derivation completes succesfully: */
	// if err := session.Transition(SessionStateConnected); err != nil {
	// 	return err
	// }
	return NewSecureConn(conn, kek)
}
