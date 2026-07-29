// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: serverTLSConfig and clientTLSConfig build the throwaway self-signed TLS
// ABOUTME: config QUIC requires at the protocol level; real security is layered on top.

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
