package session

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// PeerHandshakeMessage defines the JSON payload sent during handshake.
type PeerHandshakeMessage struct {
	Fingerprint string            `json:"fingerprint"`
	PublicKeys  map[string]string `json:"public_keys"` // base64 encoded
	EncSeeds    map[string]string `json:"enc_seeds"`   // optional for key encapsulation
}

// TODO: Check out-of-band fingerprints.

// PerformUnsecureInboundHandshake handles the first plaintext connection from Bob to Alice.
func PerformUnsecureInboundHandshake(session *Session, conn net.Conn) error {
	if session == nil || conn == nil {
		return fmt.Errorf("nil pointer deference")
	}

	logger := session.logger.New("phase", "inbound-handshake")

	// Read JSON
	var msg PeerHandshakeMessage
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		logger.Error("Failed to decode request", "error", err)
		return fmt.Errorf("invalid handshake format: %w", err)
	}

	pubKeys := make(map[string][]byte, 2)
	for k, v := range msg.PublicKeys {
		decoded, err := decodeBase64(v)
		if err != nil {
			logger.Error("Failed to base64 decode public key", "error", err)
			return fmt.Errorf("invalid base64 key for %s: %w", k, err)
		}
		pubKeys[k] = decoded
	}

	peerKeys, err := kbc.ParsePeerKeys(pubKeys)
	if err != nil {
		logger.Error("Failed to parse peer keys", "error", err)
		return err
	}

	computed, err := peerKeys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute fingerprint", "error", err)
		return fmt.Errorf("fingerprint computation failed: %w", err)
	}

	if subtle.ConstantTimeCompare([]byte(computed), []byte(session.ExpectedPeerFingerprint)) == 0 {
		logger.Error("Fingerprint missmatch")
		return fmt.Errorf("fingerprint mismatch: got %s, expected %s", computed, session.ExpectedPeerFingerprint)
	}

	if subtle.ConstantTimeCompare([]byte(msg.Fingerprint), []byte(session.ExpectedPeerFingerprint)) == 0 {
		logger.Error("Fingerprint missmatch")
		return fmt.Errorf("fingerprint mismatch: got %s, expected %s", computed, session.ExpectedPeerFingerprint)
	}

	session.PeerPubKeys = peerKeys

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
	if session == nil {
		return nil, fmt.Errorf("nil pointer deference")
	}

	logger := session.logger.New("phase", "outbound-handshake")
	conn, err := net.DialTimeout("tcp", remoteAddr, 15*time.Second)
	if err != nil {
		logger.Error("Failed to dial", "addr", remoteAddr, "error", err)
		return nil, fmt.Errorf("failed to connect to %s: %w", remoteAddr, err)
	}

	pubKeys := map[string]string{
		"x25519": base64.RawURLEncoding.EncodeToString(session.OwnKeys.X25519Public.Bytes()),
		"mlkem":  base64.RawURLEncoding.EncodeToString(session.OwnKeys.MlKemPublic.Bytes()),
	}
	encSeeds := make(map[string]string)
	for k, seed := range seeds {
		encSeeds[k] = base64.RawURLEncoding.EncodeToString(seed)
	}

	msg := PeerHandshakeMessage{
		Fingerprint: session.ExpectedPeerFingerprint,
		PublicKeys:  pubKeys,
		EncSeeds:    encSeeds,
	}

	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		err2 := conn.Close()
		if err2 != nil {
			logger.Error("Error on closed connection", "error", err2)
		}
		logger.Error("Failed write message", "error", err)
		return nil, fmt.Errorf("failed to send handshake: %w", err)
	}

	return conn, nil
}

// FinalizeInboundSession completes the inbound session setup after peer is verified.
// It decapsulates seeds, derives the SEKInbound, wraps the net.Conn in SecureConn, and finalizes state.
func FinalizeInboundSession(session *Session, conn net.Conn, encSeeds map[string]string) error {
	if session == nil || conn == nil {
		return fmt.Errorf("nil pointer deference")
	}
	logger := session.logger.New("phase", "inbound-finalize")

	// === Pre-check: make sure peer is verified ===
	if err := session.ValidatePeer(); err != nil {
		logger.Error("Failed to validate peer", "error", err)
		return fmt.Errorf("cannot finalize session: peer not verified: %w", err)
	}

	// === Step 1: Decode base64-encoded ciphertexts ===
	ctKEM_b64, ok1 := encSeeds["mlkem"]
	ctXDH_b64, ok2 := encSeeds["x25519"]
	if !ok1 || !ok2 {
		logger.Error("Missing ecnapsulated seeds", "mlkem-present", ok1, "x25519-present", ok2)
		return fmt.Errorf("missing encapsulated seeds (mlkem or x25519)")
	}

	ctKEM, err := decodeBase64(ctKEM_b64)
	if err != nil {
		logger.Error("Failed to decode mlkem seed", "error", err)
		return fmt.Errorf("failed to decode mlkem ciphertext: %w", err)
	}

	ctXDH, err := decodeBase64(ctXDH_b64)
	if err != nil {
		logger.Error("Failed to decode x25519 seed", "error", err)
		return fmt.Errorf("failed to decode x25519 ciphertext: %w", err)
	}

	// === Step 2: Decapsulate both secrets ===
	sharedKEM, err := session.OwnKeys.MlKemPrivate.Decapsulate(ctKEM)
	if err != nil {
		logger.Error("Failed to decapsulate mlkem seed", "error", err)
		return fmt.Errorf("mlkem decapsulation failed: %w", err)
	}

	seed2, err := kbc.X25519Decapsulate(ctXDH, session.OwnKeys.X25519Private, session.PeerPubKeys.X25519Public)
	if err != nil {
		logger.Error("Failed to decapsulate x25519 seed", "error", err)
		return fmt.Errorf("x25519 decapsulation failed: %w", err)
	}

	// === Step 3: Derive KEK ===
	sek, err := kbc.DeriveChaCha20Key(seed2, sharedKEM)
	if err != nil {
		logger.Error("Failed to derive SEK", "error", err)
		return fmt.Errorf("SEK derivation failed: %w", err)
	}
	session.SEKInbound = sek

	// === Step 4: Upgrade connection to SecureConn ===
	secure := NewSecureConn(conn, sek)
	session.Session = &SessionSockets{
		Inbound: secure,
	}

	// === Step 5: Transition to connected state ===
	if err := session.Transition(SessionStateConnected); err != nil {
		return fmt.Errorf("failed to transition to connected state: %w", err)
	}

	logger.Info("Inbound session finalized and secured")
	return nil
}
