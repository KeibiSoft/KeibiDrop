// ABOUTME: SecureConn wraps net.Conn with per-message AEAD encryption and re-keying support.
// ABOUTME: Provides SecureReader, SecureWriter, and SecureConn for encrypted session transport.
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
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

// MaxSecureMessageSize caps the encrypted payload a SecureReader will accept. Rejected
// before allocation, so a malicious length header on the raw stream cannot OOM us.
const MaxSecureMessageSize = 20 * 1024 * 1024 // 20 MiB

// Default rotation thresholds and the floors the debug override clamps to.
const (
	defaultRekeyBytes uint64 = 1 << 30 // 1 GB
	defaultRekeyMsgs  uint64 = 1 << 20 // ~1M messages
	// Floors for the KD_REKEY_BYTES/KD_REKEY_MSGS debug override: it may only make rotation
	// fire sooner, never disable it.
	minRekeyBytes uint64 = 1 << 20 // 1 MiB
	minRekeyMsgs  uint64 = 1 << 10 // 1024
)

// Re-keying thresholds for forward secrecy. Vars, not consts, so tests can shrink them to
// exercise the rekey path; production uses these unless the debug override is applied.
var (
	RekeyBytesThreshold = defaultRekeyBytes
	RekeyMsgsThreshold  = defaultRekeyMsgs
)

// ApplyRekeyThresholdOverride lowers the rekey thresholds for testing and benchmarking.
// A zero argument leaves that threshold unchanged; a non-zero value is clamped to
// [floor, default], so the override can only make rotation fire sooner. Returns the applied
// values. Call once at startup before the HealthMonitor goroutine reads the thresholds.
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

// encBufPool pools the per-message encrypt buffer. Reuse is safe because net.Conn.Write
// copies to kernel space.
var encBufPool = sync.Pool{
	New: func() any { return new([]byte) },
}

// SecureWriter encrypts messages and writes them to an underlying writer.
type SecureWriter struct {
	w     io.Writer
	aead  cipher.AEAD
	nonce *kbc.NonceGenerator

	// Ratchet state for the in-band rekey. ck is this direction's chaining key (CK0 is the
	// session key), epoch its current key generation. Mutated only under the writer lock.
	suite  kbc.CipherSuite
	prefix uint32
	ck     []byte
	epoch  uint16
}

// NewSecureWriterWithPrefix creates a writer with the given direction nonce prefix. The
// prefix is required (no default) so a socket's two endpoints must choose different
// prefixes; a shared default caused the nonce reuse this replaces.
func NewSecureWriterWithPrefix(w io.Writer, kek []byte, suite kbc.CipherSuite, prefix uint32) *SecureWriter {
	aead, err := kbc.NewAEAD(suite, kek)
	if err != nil {
		panic("secureconn: invalid key: " + err.Error())
	}
	return &SecureWriter{
		w:      w,
		aead:   aead,
		nonce:  kbc.NewNonceGenerator(prefix),
		suite:  suite,
		prefix: prefix,
		ck:     append([]byte(nil), kek...),
	}
}

// ratchet advances this direction to the next key epoch: derives new chaining and message
// keys (optionally folding a staged KEM secret for post-compromise healing), installs the
// new AEAD, bumps the nonce epoch (resets the counter), and zeroizes the old chaining key.
// Caller holds the writer lock, so the epoch bumps once and no Next runs between the AEAD
// swap and the counter reset.
func (sw *SecureWriter) ratchet(foldSecret []byte) error {
	next := sw.epoch + 1
	ck, mk, err := kbc.RatchetKeys(sw.ck, sw.prefix, next, foldSecret)
	if err != nil {
		return err
	}
	aead, err := kbc.NewAEAD(sw.suite, mk)
	if err != nil {
		return err
	}
	sw.aead = aead
	sw.nonce.SetEpoch(next) // resets the counter; the next Next emits (next, 1)
	zeroize(sw.ck)
	sw.ck = ck
	sw.epoch = next
	return nil
}

// zeroize best-effort wipes a chaining key after it has been ratcheted forward.
func zeroize(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func (s *SecureWriter) Write(p []byte) (int, error) {
	nonce, err := s.nonce.Next()
	if err != nil {
		return 0, err
	}

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

// foldSecret is a staged fold secret with a local monotonic generation, for deterministic retirement.
type foldSecret struct {
	gen    uint64
	secret []byte
}

// maxStagedFolds caps the un-retired fold set (safety valve; single-flight folds never reach it).
const maxStagedFolds = 64

// SecureReader reads encrypted messages and decrypts them.
type SecureReader struct {
	r    io.Reader
	aead cipher.AEAD
	head [lengthHeaderSize]byte

	// Receive ratchet state, used only when keyUpdate is on. prefix is the peer writer's
	// direction prefix, so the salt matches what the sender derived; ck is the receive
	// chaining key (CK0 is the session key); lastNonce is the last accepted nonce[4:12]
	// (epoch:counter), strictly increasing. Owned by the single-goroutine Read invariant.
	suite     kbc.CipherSuite
	prefix    uint32
	ck        []byte
	epoch     uint16
	lastNonce uint64
	keyUpdate atomic.Bool

	// Entropy-fold state. foldStaged keeps every un-retired KEM secret (oldest first); the reader
	// tries them all, so a fold burst has no window to overflow. Guarded by foldMu (staged off the
	// Read goroutine). foldCommitted latches on the first folded epoch: the responder writer gate.
	foldMu        sync.Mutex
	foldStaged    []foldSecret
	foldGen       uint64
	foldCommitted atomic.Bool
}

// stageFold appends a fold secret to try on a later epoch bump. Keeps every un-retired round:
// a burst can stage several before the peer writer folds an older one, so dropping any would make
// that frame undecryptable. Domain-separated labels mean at most one candidate authenticates.
func (s *SecureReader) stageFold(secret []byte) {
	s.foldMu.Lock()
	s.foldGen++
	s.foldStaged = append(s.foldStaged, foldSecret{gen: s.foldGen, secret: append([]byte(nil), secret...)})
	if len(s.foldStaged) > maxStagedFolds { // safety valve; single-flight folds never reach it
		zeroize(s.foldStaged[0].secret)
		s.foldStaged = s.foldStaged[1:]
	}
	s.foldMu.Unlock()
}

// loadFoldCandidates returns the un-retired fold secrets, newest first (common case matches first).
func (s *SecureReader) loadFoldCandidates() [][]byte {
	s.foldMu.Lock()
	defer s.foldMu.Unlock()
	out := make([][]byte, 0, len(s.foldStaged))
	for i := len(s.foldStaged) - 1; i >= 0; i-- {
		out = append(out, append([]byte(nil), s.foldStaged[i].secret...))
	}
	return out
}

// retireFoldAfterCommit drops the committed secret and all older: the peer writer folds
// generations monotonically, so a generation <= committed can never be folded again. Newer stay.
func (s *SecureReader) retireFoldAfterCommit(committed []byte) {
	s.foldMu.Lock()
	idx := -1
	for i := range s.foldStaged {
		if bytes.Equal(s.foldStaged[i].secret, committed) {
			idx = i
			break
		}
	}
	for i := 0; i <= idx; i++ {
		zeroize(s.foldStaged[i].secret)
	}
	if idx >= 0 {
		s.foldStaged = append(s.foldStaged[:0], s.foldStaged[idx+1:]...)
	}
	s.foldMu.Unlock()
}

func NewSecureReader(r io.Reader, kek []byte, suite kbc.CipherSuite, prefix uint32) *SecureReader {
	aead, err := kbc.NewAEAD(suite, kek)
	if err != nil {
		panic("secureconn: invalid key: " + err.Error())
	}
	return &SecureReader{
		r:      r,
		aead:   aead,
		suite:  suite,
		prefix: prefix,
		ck:     append([]byte(nil), kek...),
	}
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

	if !s.keyUpdate.Load() {
		// Old-peer / epoch-0 path: decrypt with the current key, no epoch tracking, so
		// the wire behaviour is exactly as before the ratchet existed.
		plaintext, err := s.aead.Open(ciphertext[:0], nonce, ciphertext, nil)
		if err != nil {
			return nil, fmt.Errorf("decryption failed: %w", err)
		}
		return plaintext, nil
	}
	return s.openRatcheted(nonce, ciphertext)
}

// openRatcheted decrypts a frame under the receive ratchet. Enforces the epoch and replay
// gates before any key derivation, follows a single-step epoch bump, and commits the advance
// only after the frame authenticates, so an injected epoch cannot desync the chain.
func (s *SecureReader) openRatcheted(nonce, ciphertext []byte) ([]byte, error) {
	wireEpoch := binary.BigEndian.Uint16(nonce[4:6])
	wireNonce := binary.BigEndian.Uint64(nonce[4:]) // (epoch<<48)|counter, strictly increasing

	// Gate before deriving anything: reject a far-future epoch so a forged frame cannot force
	// an unbounded run of ratchet steps, and reject a non-increasing nonce so a replay or
	// reorder never reaches the AEAD.
	if wireEpoch > s.epoch+1 {
		return nil, fmt.Errorf("rekey epoch gap: frame epoch %d, reader at %d", wireEpoch, s.epoch)
	}
	if wireNonce <= s.lastNonce {
		return nil, fmt.Errorf("rekey replay: nonce %d not greater than last %d", wireNonce, s.lastNonce)
	}

	if wireEpoch == s.epoch {
		plaintext, err := s.aead.Open(ciphertext[:0], nonce, ciphertext, nil)
		if err != nil {
			return nil, fmt.Errorf("decryption failed: %w", err)
		}
		s.lastNonce = wireNonce
		return plaintext, nil
	}

	// wireEpoch == s.epoch+1: the sender advanced one epoch, plain or folded (it mixed a
	// staged KEM secret); the wire carries no signal of which, so try each candidate and commit
	// whichever authenticates. Candidates: current staged round, superseded prior round, then
	// plain. Each opens into a FRESH buffer, never ciphertext[:0]: AEAD Open zeroes its
	// destination on failure, which would corrupt the shared ciphertext for the next candidate.
	for _, fold := range foldCandidates(s.loadFoldCandidates()) {
		ck, mk, err := kbc.RatchetKeys(s.ck, s.prefix, wireEpoch, fold)
		if err != nil {
			continue
		}
		aead, err := kbc.NewAEAD(s.suite, mk)
		if err != nil {
			continue
		}
		plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
		if err != nil {
			continue
		}

		// Authenticated: commit the advance and zeroize the retired chaining key.
		s.aead = aead
		zeroize(s.ck)
		s.ck = ck
		s.epoch = wireEpoch
		s.lastNonce = wireNonce
		if fold != nil {
			s.retireFoldAfterCommit(fold) // retire this generation and all older; keep newer
			s.foldCommitted.Store(true)   // release the responder's gated writers
		}
		return plaintext, nil
	}
	return nil, fmt.Errorf("decryption failed at epoch %d", wireEpoch)
}

// foldCandidates appends the plain (nil) candidate after the staged rounds. Distinct fold
// labels mean at most one authenticates, so the order only decides which is attempted first.
func foldCandidates(staged [][]byte) [][]byte {
	return append(staged, nil)
}

// SecureConn wraps a net.Conn with separate inbound/outbound encryption.
type SecureConn struct {
	conn         net.Conn
	suite        kbc.CipherSuite
	writerPrefix uint32 // direction nonce prefix for this endpoint's writer; persisted across rekeys
	readerPrefix uint32 // the peer writer's prefix (opposite of writerPrefix), for the receive ratchet
	r            *SecureReader
	w            *SecureWriter

	// leftover holds decrypted plaintext that didn't fit in the caller's buffer on a previous
	// Read. Not goroutine-safe: Read must be called from a single goroutine (gRPC guarantees
	// this for its connections).
	leftover  []byte
	done      atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}

	// wMu serializes the writer and its inline ratchet: holding it across the ratchet keeps
	// the epoch bump single-flight (nonce never reused) and the records ordered (so the
	// reader's monotonic epoch/counter gate holds). Separate from keyMu on purpose: Read holds
	// keyMu across a blocking network read, so an exclusive writer sharing it would deadlock.
	wMu             sync.Mutex
	useKeyUpdate    atomic.Bool // the in-band ratchet is active (both peers negotiated it)
	writerBytesMark uint64      // bytesSent at the last writer ratchet (guarded by wMu)
	writerMsgsMark  uint64      // msgsSent at the last writer ratchet (guarded by wMu)
	// stagedWriterFold is the 32-byte KEM secret to fold into this writer's next epoch bump;
	// foldInitiator marks this peer as the fold initiator, whose writers fold immediately,
	// versus a responder whose writers wait for the gate (s.r.foldCommitted). Both under wMu.
	stagedWriterFold []byte
	foldInitiator    bool
	lastRatchetNano  atomic.Int64

	// Re-keying support: track bytes/messages for forward secrecy.
	bytesSent    atomic.Uint64
	bytesRecv    atomic.Uint64
	msgsSent     atomic.Uint64
	msgsRecv     atomic.Uint64
	currentEpoch atomic.Uint64
	keyMu        sync.RWMutex // protects key updates
}

// RekeyTimeCadence bounds how long a direction may go without a ratchet even when below the
// byte/message thresholds, so a low-volume (heartbeat-only) direction still earns forward
// secrecy. A var so tests and benchmarks can shrink it.
var RekeyTimeCadence = 60 * time.Second

// epochWrapGuard is the epoch at which the writer stops the in-band ratchet, a small margin
// below the uint16 max (0xFFFF). A wrap 0xFFFF -> 0 would reset the counter under an epoch the
// reader already passed, so its monotonic gate would reject every later frame (a brick). Near
// the guard the direction holds its epoch; a re-handshake rotates the whole session on a fresh
// epoch-0 key before the hard limit.
const epochWrapGuard uint16 = 0xFFFF - 8

// epochRehandshakeThreshold is the writer epoch at which a key-update session forces a
// re-handshake for a fresh epoch-0 key, a margin below epochWrapGuard so the rotation lands
// before the ratchet stops. Without it a key-update pair would pin its epoch at the guard and
// stop advancing forward secrecy.
const epochRehandshakeThreshold uint16 = epochWrapGuard - 16

// NewSecureConn wraps conn with AEAD encryption. writerPrefix selects this endpoint's
// direction nonce prefix (NoncePrefixOutbound for the dialing side, NoncePrefixInbound for the
// accepting side). Required so the two endpoints of a socket, which share one key, cannot both
// default to the same prefix and reuse nonces.
func NewSecureConn(conn net.Conn, kek []byte, suite kbc.CipherSuite, writerPrefix uint32) *SecureConn {
	readerPrefix := oppositePrefix(writerPrefix)
	sc := &SecureConn{
		conn:         conn,
		suite:        suite,
		writerPrefix: writerPrefix,
		readerPrefix: readerPrefix,
		r:            NewSecureReader(conn, kek, suite, readerPrefix),
		w:            NewSecureWriterWithPrefix(conn, kek, suite, writerPrefix),
		closed:       make(chan struct{}),
	}
	sc.lastRatchetNano.Store(nowNano())
	return sc
}

// oppositePrefix returns the peer writer's direction prefix for a local writer prefix,
// so the receive ratchet salts with the same prefix the sender used.
func oppositePrefix(writerPrefix uint32) uint32 {
	if writerPrefix == NoncePrefixOutbound {
		return NoncePrefixInbound
	}
	return NoncePrefixOutbound
}

func nowNano() int64 { return time.Now().UnixNano() }

// ReadMessage reads and decrypts a full message.
func (s *SecureConn) ReadMessage() ([]byte, error) {
	return s.r.Read()
}

// WriteMessage encrypts and writes a full message.
func (s *SecureConn) WriteMessage(msg []byte) error {
	_, err := s.w.Write(msg)
	return err
}

// Close closes the underlying connection. Idempotent and race-safe: the proactive rekey drop
// and gRPC's transport can both close the same *SecureConn. sync.Once collapses the double
// close so close(s.closed) never panics, and the atomic done keeps Accept's read race-free.
// Only the first close reports the conn's error; later closes return net.ErrClosed.
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
	s.wMu.Lock()
	// Ratchet before sealing this frame if the direction is due, so the epoch bump rides the
	// next record (make-before-break, zero pause). All under wMu, so the epoch bumps once and
	// the seal + ordered write follow without another writer slipping in. A staged fold takes
	// priority and bumps promptly; otherwise a plain ratchet fires only on the thresholds.
	if s.useKeyUpdate.Load() {
		if fold := s.pendingWriterFoldLocked(); fold != nil {
			if err := s.w.ratchet(fold); err != nil {
				s.wMu.Unlock()
				return 0, fmt.Errorf("rekey fold ratchet failed: %w", err)
			}
			zeroize(s.stagedWriterFold)
			s.stagedWriterFold = nil // single-use
			s.postRatchetLocked()
		} else if s.writerShouldRatchet() {
			if err := s.w.ratchet(nil); err != nil {
				s.wMu.Unlock()
				return 0, fmt.Errorf("rekey ratchet failed: %w", err)
			}
			s.postRatchetLocked()
		}
	}
	n, err := s.w.Write(p)
	s.wMu.Unlock()
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

// writerShouldRatchet reports whether the send direction is due for a key rotation: it moved a
// threshold's worth of bytes or messages since the last ratchet, or the idle time cadence
// elapsed (so a heartbeat-only direction still rotates). Called under wMu.
func (s *SecureConn) writerShouldRatchet() bool {
	// Hold the epoch near the uint16 max rather than wrap it (which would brick the
	// reader's monotonic gate); the re-handshake fallback rotates before the hard limit.
	if s.w.epoch >= epochWrapGuard {
		return false
	}
	if s.bytesSent.Load()-s.writerBytesMark >= RekeyBytesThreshold ||
		s.msgsSent.Load()-s.writerMsgsMark >= RekeyMsgsThreshold {
		return true
	}
	return nowNano()-s.lastRatchetNano.Load() >= int64(RekeyTimeCadence)
}

// stageReaderFold stages the 32-byte KEM secret on this conn's receive direction so the reader
// can follow the peer's fold. Under keyMu.RLock (the same discipline as Read), so it never
// races an UpdateKey conn swap, then foldMu inside stageFold.
func (s *SecureConn) stageReaderFold(secret []byte) {
	s.keyMu.RLock()
	s.r.stageFold(secret)
	s.keyMu.RUnlock()
}

// armWriterFold stages the secret on this conn's send direction and records whether this peer
// initiated the fold (an initiator's writer folds promptly; a responder's waits for the gate).
// Under wMu. Callers must arm the writer only AFTER every reader the peer's fold-back can reach
// is staged, so no writer folds before its counterpart reader is ready.
func (s *SecureConn) armWriterFold(secret []byte, isInitiator bool) {
	s.wMu.Lock()
	zeroize(s.stagedWriterFold)
	s.stagedWriterFold = append([]byte(nil), secret...)
	s.foldInitiator = isInitiator
	s.wMu.Unlock()
}

// StageEntropyFold stages a 32-byte KEM fold secret on both directions of this conn for one
// fold round: the reader first, then the writer, so a single-conn stage holds the reader-
// before-writer invariant on its own. The writer folds it into its next epoch bump (promptly
// for the initiator, or once the gate opens for a responder); the reader folds it when it next
// follows an epoch bump. Staging is supersede-idempotent. Serialized under wMu (writer) and
// foldMu-under-keyMu.RLock (reader), so a duplicate Rekey cannot tear the state or race an
// UpdateKey conn swap.
func (s *SecureConn) StageEntropyFold(secret []byte, isInitiator bool) {
	s.stageReaderFold(secret)
	s.armWriterFold(secret, isInitiator)
}

// pendingWriterFoldLocked returns the staged fold secret if this writer is cleared to fold on
// its next bump: a fold is staged, the epoch is below the wrap guard, and this peer either
// initiated the fold or has seen the initiator's fold on its own reader (the responder gate).
// Otherwise nil. Called under wMu, so s.r is stable.
func (s *SecureConn) pendingWriterFoldLocked() []byte {
	if s.stagedWriterFold == nil || s.w.epoch >= epochWrapGuard {
		return nil
	}
	if s.foldInitiator || s.r.foldCommitted.Load() {
		return s.stagedWriterFold
	}
	return nil
}

// postRatchetLocked records the byte/message/time marks after a writer epoch bump. Under wMu.
func (s *SecureConn) postRatchetLocked() {
	s.writerBytesMark = s.bytesSent.Load()
	s.writerMsgsMark = s.msgsSent.Load()
	s.lastRatchetNano.Store(nowNano())
}

// SetKeyUpdate turns the in-band rekey ratchet on for this conn. Called once during the
// handshake, before the read and write goroutines start, when both peers advertised the
// capability. With it off the conn behaves as epoch 0 (old-peer interop) and rotates only via
// the re-handshake fallback.
func (s *SecureConn) SetKeyUpdate(on bool) {
	s.useKeyUpdate.Store(on)
	s.r.keyUpdate.Store(on)
}

// UsesKeyUpdate reports whether the in-band ratchet is active on this conn. The health monitor
// reads it to keep the legacy volume-based re-handshake trigger off ratcheting sessions: their
// byte counters are cumulative, so past one threshold ShouldRekey stays true forever, and a
// volume-driven re-handshake would tear down a healthy session that is already rotating.
func (s *SecureConn) UsesKeyUpdate() bool {
	return s.useKeyUpdate.Load()
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

// ShouldRekey returns true if key rotation is recommended. It considers BOTH directions: a
// bulk transfer bumps the sender's bytesSent and the receiver's bytesRecv, and either peer may
// drive the rekey, so both must observe that the threshold was crossed.
func (s *SecureConn) ShouldRekey() bool {
	return s.bytesSent.Load() >= RekeyBytesThreshold ||
		s.bytesRecv.Load() >= RekeyBytesThreshold ||
		s.msgsSent.Load() >= RekeyMsgsThreshold ||
		s.msgsRecv.Load() >= RekeyMsgsThreshold
}

// WriterEpoch returns the outbound key epoch. It advances each time the in-band ratchet
// rotates the send key, so a monitor or test can observe rotation without a socket drop. Safe
// on the live path: s.w is stable there (only test-only UpdateKey swaps it) and the epoch lives
// in an atomic word.
func (s *SecureConn) WriterEpoch() uint16 {
	return s.w.nonce.Epoch()
}

// SetWriterEpochForTest fast-forwards this conn's writer key epoch, for tests that need the
// epoch-wrap re-handshake path without ratcheting billions of times. Monotonic only: a backward
// or duplicate epoch would reset the counter under an unchanged key and reuse a nonce, so it
// refuses to move backward. Not safe on a live conn.
func (s *SecureConn) SetWriterEpochForTest(e uint16) {
	if e <= s.w.epoch {
		return
	}
	s.w.nonce.SetEpoch(e)
	s.w.epoch = e
}

// UpdateKey rebuilds both directions on a fresh key, resetting the ratchet to epoch 0. The
// legacy full-swap primitive kept for the re-handshake fallback and its test. Takes wMu then
// keyMu, so it is exclusive against both Write and Read. Do not call on a live reader: it holds
// wMu while waiting for keyMu behind a possibly network-blocked Read, stalling every Write until
// an inbound frame arrives. The live path rotates via the ratchet.
func (s *SecureConn) UpdateKey(newKek []byte) {
	s.wMu.Lock()
	defer s.wMu.Unlock()
	s.keyMu.Lock()
	defer s.keyMu.Unlock()

	zeroize(s.r.ck)
	zeroize(s.w.ck)
	s.r = NewSecureReader(s.conn, newKek, s.suite, s.readerPrefix)
	s.r.keyUpdate.Store(s.useKeyUpdate.Load())
	s.w = NewSecureWriterWithPrefix(s.conn, newKek, s.suite, s.writerPrefix)
	s.ResetStats()
	s.writerBytesMark = 0
	s.writerMsgsMark = 0
	// A full re-handshake swap installs a fresh session key, so any fold staged against the
	// old key is stale; drop it (the reader is rebuilt above, so its staged fold is gone too).
	zeroize(s.stagedWriterFold)
	s.stagedWriterFold = nil
	s.foldInitiator = false
	s.lastRatchetNano.Store(nowNano())
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
