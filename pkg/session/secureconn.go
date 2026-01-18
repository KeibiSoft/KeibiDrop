// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

const lengthHeaderSize = 4 // uint32 prefix

// Re-keying thresholds for forward secrecy.
const (
	RekeyBytesThreshold = 1 << 30 // 1 GB
	RekeyMsgsThreshold  = 1 << 20 // ~1M messages
)

// Nonce prefixes for direction separation (prevents nonce reuse with same key).
const (
	NoncePrefixOutbound uint32 = 0x4F555442 // "OUTB"
	NoncePrefixInbound  uint32 = 0x494E4244 // "INBD"
)

// SecureWriter encrypts messages and writes them to an underlying writer.
// Uses deterministic counter-based nonces for performance (~500ns -> ~1ns per nonce).
type SecureWriter struct {
	w     io.Writer
	kek   []byte
	nonce *kbc.NonceGenerator
}

func NewSecureWriter(w io.Writer, kek []byte) *SecureWriter {
	return &SecureWriter{
		w:     w,
		kek:   kek,
		nonce: kbc.NewNonceGenerator(NoncePrefixOutbound),
	}
}

// NewSecureWriterWithPrefix creates a writer with a custom nonce prefix.
func NewSecureWriterWithPrefix(w io.Writer, kek []byte, prefix uint32) *SecureWriter {
	return &SecureWriter{
		w:     w,
		kek:   kek,
		nonce: kbc.NewNonceGenerator(prefix),
	}
}

func (s *SecureWriter) Write(p []byte) (int, error) {
	nonce := s.nonce.Next()
	encrypted, err := kbc.EncryptWithNonce(s.kek, p, nonce)
	if err != nil {
		return 0, fmt.Errorf("encryption failed: %w", err)
	}

	//#nosec:G115 // safe cast, no TCP stream frame will be 5GB.
	length := uint32(len(encrypted))
	head := make([]byte, lengthHeaderSize)
	binary.BigEndian.PutUint32(head, length)

	// Write [length][encrypted]
	if _, err := s.w.Write(head); err != nil {
		return 0, fmt.Errorf("write length failed: %w", err)
	}
	if _, err := s.w.Write(encrypted); err != nil {
		return 0, fmt.Errorf("write data failed: %w", err)
	}

	return len(p), nil // number of plaintext bytes consumed
}

// SecureReader reads encrypted messages and decrypts them.
type SecureReader struct {
	r   io.Reader
	kek []byte
}

func NewSecureReader(r io.Reader, kek []byte) *SecureReader {
	return &SecureReader{r: r, kek: kek}
}

func (s *SecureReader) Read() ([]byte, error) {
	head := make([]byte, lengthHeaderSize)
	if _, err := io.ReadFull(s.r, head); err != nil {
		return nil, fmt.Errorf("read length failed: %w", err)
	}
	length := binary.BigEndian.Uint32(head)

	encrypted := make([]byte, length)
	if _, err := io.ReadFull(s.r, encrypted); err != nil {
		return nil, fmt.Errorf("read encrypted block failed: %w", err)
	}

	plaintext, err := kbc.Decrypt(s.kek, encrypted)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// SecureConn wraps a net.Conn with separate inbound/outbound encryption.
type SecureConn struct {
	conn net.Conn
	r    *SecureReader
	w    *SecureWriter

	readBuf *bytes.Buffer
	done    bool
	closed  chan struct{}

	// Re-keying support: track bytes/messages for forward secrecy.
	bytesSent    atomic.Uint64
	bytesRecv    atomic.Uint64
	msgsSent     atomic.Uint64
	msgsRecv     atomic.Uint64
	currentEpoch atomic.Uint64
	keyMu        sync.RWMutex // protects key updates
}

func NewSecureConn(conn net.Conn, kek []byte) *SecureConn {
	return &SecureConn{
		conn:    conn,
		r:       NewSecureReader(conn, kek),
		w:       NewSecureWriter(conn, kek),
		readBuf: bytes.NewBuffer(nil),
		closed:  make(chan struct{}),
	}
}

// ReadMessage reads and decrypts a full message.
func (s *SecureConn) ReadMessage() ([]byte, error) {
	return s.r.Read()
}

// WriteMessage encrypts and writes a full message.
func (s *SecureConn) WriteMessage(msg []byte) error {
	_, err := s.w.Write(msg)
	return err
}

// Close closes the underlying connection.
func (s *SecureConn) Close() error {
	if s.done {
		return net.ErrClosed
	}
	if s.conn != nil {
		s.done = true
		close(s.closed)
		return s.conn.Close()
	}

	s.done = true
	return net.ErrClosed
}

// RemoteAddr returns the remote network address.
func (s *SecureConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

// LocalAddr returns the local network address.
func (s *SecureConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *SecureConn) Read(p []byte) (int, error) {
	// Serve leftover decrypted data first
	if s.readBuf.Len() > 0 {
		n, err := s.readBuf.Read(p)
		s.bytesRecv.Add(uint64(n))
		return n, err
	}

	// Nothing buffered, read and decrypt a full new message
	s.keyMu.RLock()
	msg, err := s.r.Read()
	s.keyMu.RUnlock()
	if err != nil {
		return 0, err
	}

	s.msgsRecv.Add(1)
	s.readBuf.Write(msg)
	n, err := s.readBuf.Read(p)
	s.bytesRecv.Add(uint64(n))
	return n, err
}

func (s *SecureConn) Write(p []byte) (int, error) {
	s.keyMu.RLock()
	n, err := s.w.Write(p)
	s.keyMu.RUnlock()
	if err != nil {
		return 0, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	s.bytesSent.Add(uint64(n))
	s.msgsSent.Add(1)
	return n, nil
}

func (s *SecureConn) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *SecureConn) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

func (s *SecureConn) SetWriteDeadline(t time.Time) error {
	return s.conn.SetWriteDeadline(t)
}

func (s *SecureConn) Accept() (net.Conn, error) {
	if s.done {
		return nil, io.EOF
	}
	return s.conn, nil
}

func (l *SecureConn) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// ========== RE-KEYING SUPPORT ==========

// ShouldRekey returns true if key rotation is recommended.
func (s *SecureConn) ShouldRekey() bool {
	return s.bytesSent.Load() >= RekeyBytesThreshold ||
		s.msgsSent.Load() >= RekeyMsgsThreshold
}

// UpdateKey atomically updates the encryption key.
func (s *SecureConn) UpdateKey(newKek []byte) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	s.r = NewSecureReader(s.conn, newKek)
	s.w = NewSecureWriter(s.conn, newKek)
	s.ResetStats()
	s.currentEpoch.Add(1)
}

// ResetStats resets the byte/message counters after a rekey.
func (s *SecureConn) ResetStats() {
	s.bytesSent.Store(0)
	s.bytesRecv.Store(0)
	s.msgsSent.Store(0)
	s.msgsRecv.Store(0)
}

// GetEpoch returns the current key epoch.
func (s *SecureConn) GetEpoch() uint64 {
	return s.currentEpoch.Load()
}

// GetStats returns current byte/message counts for monitoring.
func (s *SecureConn) GetStats() (bytesSent, bytesRecv, msgsSent, msgsRecv uint64) {
	return s.bytesSent.Load(), s.bytesRecv.Load(), s.msgsSent.Load(), s.msgsRecv.Load()
}
