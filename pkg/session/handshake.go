package session

import (
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

// FinalizeInboundSession completes the inbound session setup after peer is verified.
// It decapsulates seeds, derives the KEK, wraps the net.Conn in SecureConn, and finalizes state.
func FinalizeInboundSession(session *Session, conn net.Conn, encSeeds map[string]string) error {
	logger := session.logger.New("phase", "inbound-finalize")

	// === Pre-check: make sure peer is verified ===
	if err := session.ValidatePeer(); err != nil {
		return fmt.Errorf("cannot finalize session: peer not verified: %w", err)
	}

	// === Step 1: Decode base64-encoded ciphertexts ===
	ctKEM_b64, ok1 := encSeeds["mlkem"]
	ctXDH_b64, ok2 := encSeeds["x25519"]
	if !ok1 || !ok2 {
		return fmt.Errorf("missing encapsulated seeds (mlkem or x25519)")
	}

	ctKEM, err := base64.StdEncoding.DecodeString(ctKEM_b64)
	if err != nil {
		return fmt.Errorf("failed to decode mlkem ciphertext: %w", err)
	}

	ctXDH, err := base64.StdEncoding.DecodeString(ctXDH_b64)
	if err != nil {
		return fmt.Errorf("failed to decode x25519 ciphertext: %w", err)
	}

	// === Step 2: Decapsulate both secrets ===
	sharedKEM, err := session.OwnKeys.MlKemPrivate.Decapsulate(ctKEM)
	if err != nil {
		return fmt.Errorf("mlkem decapsulation failed: %w", err)
	}

	// Deserialize peer's X25519 pubkey from []byte to *ecdh.PublicKey
	curve := session.OwnKeys.X25519Private.Curve()
	pubKeyBytes := session.PeerPubKeys["x25519"]
	if pubKeyBytes == nil {
		return fmt.Errorf("missing peer x25519 public key")
	}
	peerPubKey, err := curve.NewPublicKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("failed to parse peer x25519 pubkey: %w", err)
	}

	seed2, err := kbc.X25519Decapsulate(ctXDH, session.OwnKeys.X25519Private, peerPubKey)
	if err != nil {
		return fmt.Errorf("x25519 decapsulation failed: %w", err)
	}

	// === Step 3: Derive KEK ===
	kek, err := kbc.DeriveChaCha20Key(seed2, sharedKEM)
	if err != nil {
		return fmt.Errorf("KEK derivation failed: %w", err)
	}
	session.KEK = kek

	// === Step 4: Upgrade connection to SecureConn ===
	secure := NewSecureConn(conn, kek)
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
