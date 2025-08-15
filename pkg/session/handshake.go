package session

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// PeerHandshakeMessage defines the JSON payload sent during handshake.
type PeerHandshakeMessage struct {
	Fingerprint  string            `json:"fingerprint"`
	PublicKeys   map[string]string `json:"public_keys"` // base64 encoded
	EncSeeds     map[string]string `json:"enc_seeds"`   // optional for key encapsulation
	OutboundPort int               `json:"port"`
}

// PerformInboundHandshake handles the first plaintext connection from Bob to Alice.
func PerformInboundHandshake(session *Session, conn net.Conn) error {
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

	if subtle.ConstantTimeCompare([]byte(computed), []byte(session.ExpectedPeerFingerprint)) != 1 {
		logger.Error("Fingerprint missmatch")
		return fmt.Errorf("fingerprint mismatch: got %s, expected %s", computed, session.ExpectedPeerFingerprint)
	}

	if msg.OutboundPort < 26000 || msg.OutboundPort > 27000 {
		logger.Warn("Provided outbound port is out of known range, defaulting to config", "provided-port", msg.OutboundPort, "default-to", config.OutboundPort)
		msg.OutboundPort = config.OutboundPort
	}

	session.PeerPubKeys = peerKeys
	session.PeerPort = msg.OutboundPort

	// Wait for user to confirm out-of-band fingerprint
	logger.Info("Peer fingerprint verified, awaiting user confirmation", "peer-port", session.PeerPort)

	// TODO: Uncomment this and get permission from User.
	/*
		// In real UI, this would be blocking for user approval
		if err := session.Transition(SessionStateVerified); err != nil {
			logger.Error("Failed to transition session state", "error", err)
			return err
		}
	*/
	return nil
}

// PerformOutboundHandshake connects Alice to Bob and sends her handshake.
func PerformOutboundHandshake(session *Session, remoteAddr string) error {
	if session == nil || session.OwnKeys == nil || session.PeerPubKeys == nil {
		return fmt.Errorf("nil pointer dereference")
	}

	logger := session.logger.New("phase", "outbound-handshake")

	seed1 := kbc.GenerateSeed()
	encSeed1X25519, err := kbc.X25519Encapsulate(seed1, session.OwnKeys.X25519Private, session.PeerPubKeys.X25519Public)
	if err != nil {
		logger.Error("Failed to encapsulate x25519 seed", "error", err)
		return err
	}

	seed2, encSeed2MLKEM := session.PeerPubKeys.MlKemPublic.Encapsulate()

	outboundKey, err := kbc.DeriveChaCha20Key(seed1, seed2)
	if err != nil {
		logger.Error("Failed to derive outbound key", "error", err)
		return err
	}

	session.SEKOutbound = outboundKey

	conn, err := net.DialTimeout("tcp", remoteAddr, 15*time.Second)
	if err != nil {
		logger.Error("Failed to dial", "addr", remoteAddr, "error", err)
		return fmt.Errorf("failed to connect to %s: %w", remoteAddr, err)
	}

	pubKeys := map[string]string{
		"x25519": encodeBase64(session.OwnKeys.X25519Public.Bytes()),
		"mlkem":  encodeBase64(session.OwnKeys.MlKemPublic.Bytes()),
	}

	encSeeds := map[string]string{
		"mlkem":  encodeBase64(encSeed2MLKEM),
		"x25519": encodeBase64(encSeed1X25519),
	}

	msg := PeerHandshakeMessage{
		Fingerprint:  session.ExpectedPeerFingerprint,
		PublicKeys:   pubKeys,
		EncSeeds:     encSeeds,
		OutboundPort: session.DefaultInboundPort,
	}

	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		_ = conn.Close()
		logger.Error("Failed to send handshake", "error", err)
		return fmt.Errorf("failed to send handshake: %w", err)
	}

	// TODO: Review the commented out code.
	/*
		// Await confirmation from Bob that he's happy
		ack := make([]byte, 2)
		if _, err := io.ReadFull(conn, ack); err != nil || string(ack) != "OK" {
			_ = conn.Close()
			logger.Error("Did not receive 'OK' from peer", "got", string(ack), "error", err)
			return fmt.Errorf("handshake rejected or invalid response")
		}


	*/
	logger.Info("Peer confirmed handshake upgrading to encrypted connection")

	// Upgrade to SecureConn
	secure := NewSecureConn(conn, session.SEKOutbound)
	if session.Session == nil {
		session.Session = &SessionSockets{}
	}
	session.Session.Outbound = secure

	return nil
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

	seed1, err := kbc.X25519Decapsulate(ctXDH, session.OwnKeys.X25519Private, session.PeerPubKeys.X25519Public)
	if err != nil {
		logger.Error("Failed to decapsulate x25519 seed", "error", err)
		return fmt.Errorf("x25519 decapsulation failed: %w", err)
	}

	// === Step 3: Derive KEK ===
	sek, err := kbc.DeriveChaCha20Key(seed1, sharedKEM)
	if err != nil {
		logger.Error("Failed to derive SEK", "error", err)
		return fmt.Errorf("SEK derivation failed: %w", err)
	}
	session.SEKInbound = sek

	// === Step 4: Upgrade connection to SecureConn ===
	secure := NewSecureConn(conn, sek)
	if session.Session == nil {
		session.Session = &SessionSockets{}
	}
	session.Session.Inbound = secure

	// TODO: Uncomment this and do the transition.
	/*
		// === Step 5: Transition to connected state ===
		if err := session.Transition(SessionStateConnected); err != nil {
			return fmt.Errorf("failed to transition to connected state: %w", err)
		}
	*/
	logger.Info("Inbound session finalized and secured")

	return nil
}
