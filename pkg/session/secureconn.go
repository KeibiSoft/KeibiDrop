// ABOUTME: SecureConn wraps net.Conn with per-message AEAD encryption and re-keying support.
// ABOUTME: Provides SecureReader, SecureWriter, and SecureConn for encrypted session transport.
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"crypto/cipher"
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

// MaxSecureMessageSize caps the encrypted payload a SecureReader will accept.
// Anything larger is rejected before allocation, preventing OOM from a
// malicious length header on the raw TCP stream.
const MaxSecureMessageSize = 20 * 1024 * 1024 // 20 MiB

// Default rotation thresholds and the floors the debug override clamps to.
const (
	defaultRekeyBytes uint64 = 1 << 30 // 1 GB
	defaultRekeyMsgs  uint64 = 1 << 20 // ~1M messages
	// Floors for the KD_REKEY_BYTES/KD_REKEY_MSGS debug override: it may only make
	// rotation fire sooner, never disable it or turn a tiny value into a per-tick storm.
	minRekeyBytes uint64 = 1 << 20 // 1 MiB
	minRekeyMsgs  uint64 = 1 << 10 // 1024
)

// Re-keying thresholds for forward secrecy. Vars, not consts, so tests can shrink
// them to exercise the rekey path without moving a gigabyte; production always uses
// these values unless the debug override below is applied.
var (
	RekeyBytesThreshold = defaultRekeyBytes
	RekeyMsgsThreshold  = defaultRekeyMsgs
)

// ApplyRekeyThresholdOverride lowers the rekey thresholds for testing and benchmarking.
// A zero argument leaves that threshold unchanged; a non-zero value is clamped to
// [floor, default], so the override can only make rotation fire sooner, never disable it.
// It returns the applied values. Call once at startup, gated on the proactive-rekey
// flag, before the HealthMonitor goroutine starts reading the thresholds.
func ApplyRekeyThresholdOverride(bytes, msgs uint64) (uint64, uint64) {
	if bytes != 0 {
		RekeyBytesThreshold = clampUint64(bytes, minRekeyBytes, defaultRekeyBytes)
	}
	if msgs != 0 {
		RekeyMsgsThreshold = clampUint64(msgs, minRekeyMsgs, defaultRekeyMsgs)
	}
	return RekeyBytesThreshold, RekeyMsgsThreshold
}

func clampUint64(v, lo, hi uint64) uint64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Nonce prefixes for direction separation (prevents nonce reuse with same key).
const (
	NoncePrefixOutbound uint32 = 0x4F555442 // "OUTB"
	NoncePrefixInbound  uint32 = 0x494E4244 // "INBD"
)

// encBufPool pools the per-message encrypt buffer to reduce GC pressure.
// Buffers are safe to reuse because net.Conn.Write copies to kernel space.
var encBufPool = sync.Pool{
	New: func() any { return new([]byte) },
}

// SecureWriter encrypts messages and writes them to an underlying writer.
// Uses a cached AEAD cipher and single combined write for performance.
type SecureWriter struct {
	w     io.Writer
	aead  cipher.AEAD
	nonce *kbc.NonceGenerator
}

// NewSecureWriterWithPrefix creates a writer with the given direction nonce prefix.
// The prefix is required (there is no default) so that a socket's two endpoints must
// choose different prefixes; a shared default is what caused the nonce reuse this
// replaces.
func NewSecureWriterWithPrefix(w io.Writer, kek []byte, suite kbc.CipherSuite, prefix uint32) *SecureWriter {
	aead, err := kbc.NewAEAD(suite, kek)
	if err != nil {
		panic("secureconn: invalid key: " + err.Error())
	}
	return &SecureWriter{
		w:     w,
		aead:  aead,
		nonce: kbc.NewNonceGenerator(prefix),
	}
}

func (s *SecureWriter) Write(p []byte) (int, error) {
	nonce := s.nonce.Next()

	// Layout: [4-byte length header][nonce][ciphertext+tag]
	// Single allocation, single write to avoid wasting a TCP segment on the header.
	encSize := kbc.NonceSize + len(p) + s.aead.Overhead()
	totalSize := lengthHeaderSize + encSize

	var buf []byte
	if bp, ok := encBufPool.Get().(*[]byte); ok && cap(*bp) >= totalSize {
		buf = (*bp)[:totalSize]
	} else {
		buf = make([]byte, totalSize)
	}

	//#nosec:G115 // safe cast, no TCP stream frame will be 5GB.
	binary.BigEndian.PutUint32(buf[:lengthHeaderSize], uint32(encSize))
	copy(buf[lengthHeaderSize:], nonce[:])
	s.aead.Seal(buf[lengthHeaderSize+kbc.NonceSize:lengthHeaderSize+kbc.NonceSize], nonce[:], p, nil)

	defer func() { b := buf[:0]; encBufPool.Put(&b) }()
	if _, err := s.w.Write(buf); err != nil {
		return 0, fmt.Errorf("write failed: %w", err)
	}

	return len(p), nil // number of plaintext bytes consumed
}

// SecureReader reads encrypted messages and decrypts them.
// Caches the AEAD cipher and reuses a header buffer.
type SecureReader struct {
	r    io.Reader
	aead cipher.AEAD
	head [lengthHeaderSize]byte
}

func NewSecureReader(r io.Reader, kek []byte, suite kbc.CipherSuite) *SecureReader {
	aead, err := kbc.NewAEAD(suite, kek)
	if err != nil {
		panic("secureconn: invalid key: " + err.Error())
	}
	return &SecureReader{r: r, aead: aead}
}

func (s *SecureReader) Read() ([]byte, error) {
	if _, err := io.ReadFull(s.r, s.head[:]); err != nil {
		return nil, fmt.Errorf("read length failed: %w", err)
	}
	length := binary.BigEndian.Uint32(s.head[:])

	if length > MaxSecureMessageSize {
		return nil, fmt.Errorf("encrypted message length %d exceeds maximum %d", length, MaxSecureMessageSize)
	}
	if length < uint32(kbc.NonceSize)+uint32(s.aead.Overhead()) { //nolint:gosec // G115: NonceSize and Overhead are small constants
		return nil, fmt.Errorf("encrypted message too short: %d bytes", length)
	}

	encrypted := make([]byte, length)
	if _, err := io.ReadFull(s.r, encrypted); err != nil {
		return nil, fmt.Errorf("read encrypted block failed: %w", err)
	}

	nonce := encrypted[:kbc.NonceSize]
	ciphertext := encrypted[kbc.NonceSize:]

	plaintext, err := s.aead.Open(ciphertext[:0], nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %w", err)
	}

	return plaintext, nil
}

// SecureConn wraps a net.Conn with separate inbound/outbound encryption.
type SecureConn struct {
	conn         net.Conn
	suite        kbc.CipherSuite
	writerPrefix uint32 // direction nonce prefix for this endpoint's writer; persisted across rekeys
	r            *SecureReader
	w            *SecureWriter

	// leftover holds decrypted plaintext that didn't fit in the caller's buffer on a
	// previous Read. Not goroutine-safe: Read must be called from a single goroutine
	// (gRPC's transport guarantees this for its connections).
	leftover  []byte
	done      atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}

	// Re-keying support: track bytes/messages for forward secrecy.
	bytesSent    atomic.Uint64
	bytesRecv    atomic.Uint64
	msgsSent     atomic.Uint64
	msgsRecv     atomic.Uint64
	currentEpoch atomic.Uint64
	keyMu        sync.RWMutex // protects key updates
}

// NewSecureConn wraps conn with AEAD encryption. writerPrefix selects this endpoint's
// direction nonce prefix (NoncePrefixOutbound for the dialing/outbound side,
// NoncePrefixInbound for the accepting/inbound side). It is required precisely so the
// two endpoints of a socket, which share one key, cannot both default to the same
// prefix and reuse nonces.
func NewSecureConn(conn net.Conn, kek []byte, suite kbc.CipherSuite, writerPrefix uint32) *SecureConn {
	return &SecureConn{
		conn:         conn,
		suite:        suite,
		writerPrefix: writerPrefix,
		r:            NewSecureReader(conn, kek, suite),
		w:            NewSecureWriterWithPrefix(conn, kek, suite, writerPrefix),
		closed:       make(chan struct{}),
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

// Close closes the underlying connection. It is idempotent and race-safe: the proactive
// rekey drop (resilience.onRekeyNeeded) and gRPC's transport can both close the same
// *SecureConn. sync.Once collapses the double close so close(s.closed) never panics, and
// the atomic done keeps Accept's read race-free. Only the first close reports the
// underlying conn's error; later closes return net.ErrClosed.
func (s *SecureConn) Close() error {
	err := net.ErrClosed
	s.closeOnce.Do(func() {
		s.done.Store(true)
		if s.conn != nil {
			close(s.closed)
			err = s.conn.Close()
		}
	})
	return err
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
	if len(s.leftover) > 0 {
		n := copy(p, s.leftover)
		s.leftover = s.leftover[n:]
		if len(s.leftover) == 0 {
			s.leftover = nil
		}
		s.bytesRecv.Add(uint64(n)) //nolint:gosec // G115: n is non-negative copy count
		return n, nil
	}

	s.keyMu.RLock()
	msg, err := s.r.Read()
	s.keyMu.RUnlock()
	if err != nil {
		return 0, err
	}

	s.msgsRecv.Add(1)
	n := copy(p, msg)
	if n < len(msg) {
		s.leftover = msg[n:]
	}
	s.bytesRecv.Add(uint64(n)) //nolint:gosec // G115: n is non-negative copy count
	return n, nil
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
	s.bytesSent.Add(uint64(n)) //nolint:gosec // G115: n is non-negative write count
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
	if s.done.Load() {
		return nil, io.EOF
	}
	return s.conn, nil
}

func (l *SecureConn) Addr() net.Addr {
	return l.conn.LocalAddr()
}

// ========== RE-KEYING SUPPORT ==========

// ShouldRekey returns true if key rotation is recommended. It considers BOTH
// directions: a bulk transfer bumps the sender's bytesSent and the receiver's
// bytesRecv, and either peer may be the one that drives the rekey, so both must be
// able to observe that the threshold was crossed.
func (s *SecureConn) ShouldRekey() bool {
	return s.bytesSent.Load() >= RekeyBytesThreshold ||
		s.bytesRecv.Load() >= RekeyBytesThreshold ||
		s.msgsSent.Load() >= RekeyMsgsThreshold ||
		s.msgsRecv.Load() >= RekeyMsgsThreshold
}

// UpdateKey atomically updates the encryption key (reuses negotiated cipher suite).
func (s *SecureConn) UpdateKey(newKek []byte) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	s.r = NewSecureReader(s.conn, newKek, s.suite)
	s.w = NewSecureWriterWithPrefix(s.conn, newKek, s.suite, s.writerPrefix)
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
