package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
)

// alpnGRPCQUIC is the ALPN token both ends must agree on. QUIC mandates ALPN
// (unlike TCP where it is optional); a mismatch fails the handshake.
const alpnGRPCQUIC = "kd-grpc-quic"

// serverTLSConfig builds a throwaway, in-memory, self-signed ed25519 cert.
// QUIC requires TLS 1.3 at the protocol level, so a cert is unavoidable — but
// this one carries no identity and is never verified. Real confidentiality and
// peer authentication come from the PQC handshake + SecureConn layer we add in
// Phase 2; this exists only to satisfy QUIC. No files on disk, by design.
func serverTLSConfig() *tls.Config {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	tmpl := x509.Certificate{SerialNumber: big.NewInt(1)}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, priv.Public(), priv)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: priv}},
		NextProtos:   []string{alpnGRPCQUIC},
	}
}

// clientTLSConfig skips verification (the server cert is a throwaway) and sets
// the matching ALPN.
func clientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{alpnGRPCQUIC},
	}
}
