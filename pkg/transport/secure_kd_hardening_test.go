//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"bytes"
	"crypto/rand"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// memConn adapts an io.Reader/Writer to net.Conn so the record layer can be driven
// over an in-memory buffer (deadlines/addrs are no-ops we never exercise here).
type memConn struct {
	r io.Reader
	w io.Writer
}

func (m memConn) Read(p []byte) (int, error)     { return m.r.Read(p) }
func (m memConn) Write(p []byte) (int, error)    { return m.w.Write(p) }
func (memConn) Close() error                     { return nil }
func (memConn) LocalAddr() net.Addr              { return nil }
func (memConn) RemoteAddr() net.Addr             { return nil }
func (memConn) SetDeadline(time.Time) error      { return nil }
func (memConn) SetReadDeadline(time.Time) error  { return nil }
func (memConn) SetWriteDeadline(time.Time) error { return nil }

// tapConn records every byte written through it before passing it to the real conn,
// so a test can inspect exactly what went on the wire.
type tapConn struct {
	net.Conn
	mu    sync.Mutex
	wrote []byte
}

func (t *tapConn) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.wrote = append(t.wrote, p...)
	t.mu.Unlock()
	return t.Conn.Write(p)
}

func (t *tapConn) sent() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.wrote...)
}

// TestKDWireIsCiphertext checks SecureConn encrypts: a plaintext marker the client writes
// must not appear in the raw wire bytes but must decrypt on the server. Runs over a
// plaintext pipe so the only thing encrypting is our layer.
func TestKDWireIsCiphertext(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()

	c, s := net.Pipe()
	tap := &tapConn{Conn: c}

	srv := make(chan net.Conn, 1)
	go func() {
		sec, err := SecureKD(s, serverID, clientID.Fingerprint(), RoleServer)
		if err != nil {
			srv <- nil
			return
		}
		srv <- sec
	}()

	clientSec, err := SecureKD(tap, clientID, serverID.Fingerprint(), RoleClient)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	serverSec := <-srv
	if serverSec == nil {
		t.Fatal("server handshake failed")
	}

	marker := []byte("TOP-SECRET-PLAINTEXT-MARKER-9F3A")
	go func() { _, _ = clientSec.Write(marker) }()
	got := make([]byte, len(marker))
	if _, err := io.ReadFull(serverSec, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Fatal("server did not decrypt the marker")
	}
	if bytes.Contains(tap.sent(), marker) {
		t.Fatal("plaintext marker found on the wire: SecureConn did not encrypt the payload")
	}
}

// TestKDTamperDetected proves AEAD integrity: flipping one byte of a sealed frame
// makes the reader reject it instead of returning corrupted plaintext.
func TestKDTamperDetected(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	wc, err := newSecureConnSuite(memConn{w: &buf}, kbc.CipherChaCha20, key, key)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := wc.Write([]byte("integrity-must-hold")); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw := buf.Bytes()
	if len(raw) <= 4+12 {
		t.Fatalf("frame too short: %d", len(raw))
	}
	raw[len(raw)-1] ^= 0x01 // corrupt one byte inside ciphertext+tag

	rc, err := newSecureConnSuite(memConn{r: bytes.NewReader(raw)}, kbc.CipherChaCha20, key, key)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	if _, err := rc.Read(make([]byte, 64)); err == nil {
		t.Fatal("tampered ciphertext was accepted: AEAD integrity check is not effective")
	}
}

// TestKDBothCipherSuites checks the record layer for both negotiable ciphers, covering
// AES-256-GCM and ChaCha20-Poly1305 wire formats explicitly.
func TestKDBothCipherSuites(t *testing.T) {
	for _, suite := range []kbc.CipherSuite{kbc.CipherChaCha20, kbc.CipherAES256} {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		c, s := net.Pipe()
		wc, err := newSecureConnSuite(c, suite, key, key)
		if err != nil {
			t.Fatalf("[%s] writer: %v", suite, err)
		}
		rc, err := newSecureConnSuite(s, suite, key, key)
		if err != nil {
			t.Fatalf("[%s] reader: %v", suite, err)
		}
		msg := []byte("suite round-trip " + string(suite))
		go func() { _, _ = wc.Write(msg) }()
		got := make([]byte, len(msg))
		if _, err := io.ReadFull(rc, got); err != nil {
			t.Fatalf("[%s] read: %v", suite, err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("[%s] round-trip mismatch", suite)
		}
		_ = c.Close()
		_ = s.Close()
	}
}

// TestKDSeedDerivation checks the KEM exchange is correct and directional: encap and
// decap yield the same 32-byte key, and two independent encapsulations yield different
// keys, so the two send directions are independently keyed.
func TestKDSeedDerivation(t *testing.T) {
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	peerB := &kbc.PeerKeys{MlKemPublic: b.keys.MlKemPublic, X25519Public: b.keys.X25519Public}
	peerA := &kbc.PeerKeys{MlKemPublic: a.keys.MlKemPublic, X25519Public: a.keys.X25519Public}

	k1, encX, encK, err := encapSeed(a.keys, peerB, kbc.CipherChaCha20)
	if err != nil {
		t.Fatalf("encap: %v", err)
	}
	k1b, err := decapSeed(b.keys, peerA, kbc.CipherChaCha20, encX, encK)
	if err != nil {
		t.Fatalf("decap: %v", err)
	}
	if !bytes.Equal(k1, k1b) {
		t.Fatal("encap and decap derived different keys")
	}
	if len(k1) != 32 {
		t.Fatalf("key size %d, want 32", len(k1))
	}

	k2, _, _, err := encapSeed(a.keys, peerB, kbc.CipherChaCha20)
	if err != nil {
		t.Fatalf("encap 2: %v", err)
	}
	if bytes.Equal(k1, k2) {
		t.Fatal("two independent encapsulations produced the same key")
	}
}
