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
	_ = ln.(*net.TCPListener).SetDeadline(deadline)

	logger.Info("Listening for peer", "addr", addr)
	conn, err := ln.Accept()
	if err != nil {
		logger.Error("Failed to accept", "err", err)
		return fmt.Errorf("failed to accept connection: %w", err)
	}
	logger.Info("TCP connection accepted", "remote", conn.RemoteAddr().String())

	// Step 1: Verify peer identity + fingerprint
	err = PerformUnsecureInboundHandshake(session, conn)
	if err != nil {
		session.MarkError(fmt.Errorf("handshake failed: %w", err))
		conn.Close()
		return err
	}

	// Step 2: Decode the full handshake message (again) to extract encapsulated seeds
	var msg PeerHandshakeMessage
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		session.MarkError(fmt.Errorf("failed to decode encapsulated seeds: %w", err))
		conn.Close()
		return err
	}

	// Step 3: Derive KEK and secure the connection
	err = FinalizeInboundSession(session, conn, msg.EncSeeds)
	if err != nil {
		session.MarkError(fmt.Errorf("session finalization failed: %w", err))
		conn.Close()
		return err
	}

	logger.Info("Inbound session fully established and encrypted")
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
