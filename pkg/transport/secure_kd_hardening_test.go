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
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
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
	testkit.Run(t, func() error {
		serverID, _ := NewIdentity()
		clientID, _ := NewIdentity()

		c, s := net.Pipe()
		tap := &tapConn{Conn: c}

		var serverSec net.Conn
		joinServer := testkit.Go(func() error {
			sec, err := SecureKD(s, serverID, clientID.Fingerprint(), RoleServer)
			if err != nil {
				return nil
			}
			serverSec = sec
			return nil
		})

		clientSec := testkit.Must(SecureKD(tap, clientID, serverID.Fingerprint(), RoleClient))
		_ = joinServer()
		if err := fp.True("server handshake succeeded", serverSec != nil); err != nil {
			return err
		}

		marker := []byte("TOP-SECRET-PLAINTEXT-MARKER-9F3A")
		go func() { _, _ = clientSec.Write(marker) }()
		got := make([]byte, len(marker))
		testkit.Must(io.ReadFull(serverSec, got))
		if err := fp.BytesEqual("decrypted marker", got, marker); err != nil {
			return err
		}
		return fp.False("plaintext marker found on the wire (SecureConn did not encrypt)", bytes.Contains(tap.sent(), marker))
	})
}

// TestKDTamperDetected proves AEAD integrity: flipping one byte of a sealed frame
// makes the reader reject it instead of returning corrupted plaintext.
func TestKDTamperDetected(t *testing.T) {
	testkit.Run(t, func() error {
		key := make([]byte, 32)
		testkit.Must(rand.Read(key))

		var buf bytes.Buffer
		wc := testkit.Must(newSecureConnSuite(memConn{w: &buf}, kbc.CipherChaCha20, key, key))
		testkit.Must(wc.Write([]byte("integrity-must-hold")))

		raw := buf.Bytes()
		if err := fp.True("frame is long enough to corrupt", len(raw) > 4+12); err != nil {
			return err
		}
		raw[len(raw)-1] ^= 0x01 // corrupt one byte inside ciphertext+tag

		rc := testkit.Must(newSecureConnSuite(memConn{r: bytes.NewReader(raw)}, kbc.CipherChaCha20, key, key))
		_, err := rc.Read(make([]byte, 64))
		return fp.WantErr("tampered ciphertext rejected (AEAD integrity check)", err)
	})
}

// TestKDBothCipherSuites checks the record layer for both negotiable ciphers, covering
// AES-256-GCM and ChaCha20-Poly1305 wire formats explicitly.
func TestKDBothCipherSuites(t *testing.T) {
	testkit.Run(t, func() error {
		suites := []kbc.CipherSuite{kbc.CipherChaCha20, kbc.CipherAES256}
		return fp.Each("cipher suite round trip", suites, func(suite kbc.CipherSuite) error {
			key := make([]byte, 32)
			testkit.Must(rand.Read(key))
			c, s := net.Pipe()
			defer c.Close()
			defer s.Close()
			wc := testkit.Must(newSecureConnSuite(c, suite, key, key))
			rc := testkit.Must(newSecureConnSuite(s, suite, key, key))
			msg := []byte("suite round-trip " + string(suite))
			go func() { _, _ = wc.Write(msg) }()
			got := make([]byte, len(msg))
			testkit.Must(io.ReadFull(rc, got))
			return fp.BytesEqual(fmt.Sprintf("[%s] round trip", suite), got, msg)
		})
	})
}

// TestKDSeedDerivation checks the KEM exchange is correct and directional: encap and
// decap yield the same 32-byte key, and two independent encapsulations yield different
// keys, so the two send directions are independently keyed.
func TestKDSeedDerivation(t *testing.T) {
	testkit.Run(t, func() error {
		a, _ := NewIdentity()
		b, _ := NewIdentity()
		peerB := &kbc.PeerKeys{MlKemPublic: b.keys.MlKemPublic, X25519Public: b.keys.X25519Public}
		peerA := &kbc.PeerKeys{MlKemPublic: a.keys.MlKemPublic, X25519Public: a.keys.X25519Public}

		k1, encX, encK, err := encapSeed(a.keys, peerB, kbc.CipherChaCha20)
		if err != nil {
			return fmt.Errorf("encap: %w", err)
		}
		k1b, err := decapSeed(b.keys, peerA, kbc.CipherChaCha20, encX, encK)
		if err != nil {
			return fmt.Errorf("decap: %w", err)
		}
		if err := fp.All(
			fp.BytesEqual("encap/decap derived key", k1b, k1),
			fp.Equal("key size", len(k1), 32),
		); err != nil {
			return err
		}

		k2, _, _, err := encapSeed(a.keys, peerB, kbc.CipherChaCha20)
		if err != nil {
			return fmt.Errorf("encap 2: %w", err)
		}
		return fp.NotEqual("two independent encapsulations", string(k2), string(k1))
	})
}
