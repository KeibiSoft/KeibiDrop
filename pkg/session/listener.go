package session

import (
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"
)

// StartListener starts a TCP listener on the given port and waits for Bob.
// It will block until Bob connects and sends valid keys that match the expected fingerprint.
func StartListener(session *Session, port int) error {
	logger := session.logger.New("method", "start-listener")
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("Failed to listen", "addr", addr, "err", err)
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}
	defer ln.Close()

	deadline := time.Now().Add(10 * time.Minute)
	_ = ln.(*net.TCPListener).SetDeadline(deadline) // enforce timeout on Accept

	logger.Info("Listening for peer", "addr", addr)
	conn, err := ln.Accept()
	if err != nil {
		logger.Error("Failed to accept", "err", err)
		return fmt.Errorf("failed to accept connection: %w", err)
	}
	// TODO: Use the handshake.
	err = PerformUnsecureInboundHandshake(session, conn)
	if err != nil {
		session.MarkError(fmt.Errorf("inbound hanshake failed: %w", err))
		conn.Close()
		return err
	}
	session.Session.Inbound.conn = conn
	logger.Info("Connection accepted", "remote", session.Session.Inbound.conn.RemoteAddr().String())

	if err := session.ValidatePeer(); err != nil {
		return err
	}

	// Read handshake message
	decoder := json.NewDecoder(conn)
	var msg PeerHandshakeMessage
	if err := decoder.Decode(&msg); err != nil {
		session.MarkError(fmt.Errorf("invalid handshake message: %w", err))
		logger.Error("Invalid handshake message", "err", err)
		return err
	}

	computedFingerprint, err := ComputeFingerprintFromBase64Keys(msg.PublicKeys)
	if err != nil {
		session.MarkError(fmt.Errorf("failed to compute fingerprint: %w", err))
		logger.Error("Fingerprint computation failed", "err", err)
		return err
	}

	if computedFingerprint != session.ExpectedPeerFingerprint {
		session.MarkError(errors.New("fingerprint mismatch"))
		logger.Warn("Fingerprint mismatch", "expected", session.ExpectedPeerFingerprint, "got", computedFingerprint)
		return fmt.Errorf("fingerprint mismatch: got %s, expected %s", computedFingerprint, session.ExpectedPeerFingerprint)
	}

	session.PeerFingerprint = msg.Fingerprint
	session.PeerPubKeys = make(map[string][]byte)
	for k, v := range msg.PublicKeys {
		decoded, err := decodeBase64(v)
		if err != nil {
			session.MarkError(fmt.Errorf("invalid base64 key for %s: %w", k, err))
			logger.Error("Base64 decode failed", "key", k, "err", err)
			return err
		}
		session.PeerPubKeys[k] = decoded
	}

	logger.Info("Peer verified", "fingerprint", computedFingerprint)
	_, _ = conn.Write([]byte("OK\n"))
	if err := session.Transition(SessionStateVerified); err != nil {
		return err
	}
	return nil
}

func decodeBase64(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func ComputeFingerprintFromBase64Keys(pubKeys map[string]string) (string, error) {
	mlkemEncoded, ok1 := pubKeys["mlkem"]
	curveEncoded, ok2 := pubKeys["x25519"]
	if !ok1 || !ok2 {
		return "", errors.New("missing required public keys for fingerprint")
	}

	mlkemBytes, err := decodeBase64(mlkemEncoded)
	if err != nil {
		return "", fmt.Errorf("invalid base64 mlkem key: %w", err)
	}
	curveBytes, err := decodeBase64(curveEncoded)
	if err != nil {
		return "", fmt.Errorf("invalid base64 x25519 key: %w", err)
	}

	mlkemHash := sha512Sum(mlkemBytes)
	curveHash := sha512Sum(curveBytes)

	combined := append(mlkemHash, curveHash...)
	totalHash := sha512Sum(combined)

	return base64.StdEncoding.EncodeToString(totalHash), nil
}

func sha512Sum(data []byte) []byte {
	h := sha512.New()
	h.Write(data)
	return h.Sum(nil)
}
