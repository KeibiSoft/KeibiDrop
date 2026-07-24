package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
)

// alpnGRPCQUIC is the ALPN token both ends must agree on.
const alpnGRPCQUIC = "kd-grpc-quic"

// serverTLSConfig returns a throwaway self-signed cert. QUIC requires TLS 1.3;
// peer auth and confidentiality are the SecureConn layer, not this cert.
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

func clientTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // G402: throwaway QUIC cert, auth is the SecureConn layer
		NextProtos:         []string{alpnGRPCQUIC},
	}
}
