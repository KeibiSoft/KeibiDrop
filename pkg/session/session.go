// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

type SessionState string

const (
	SessionInit           SessionState = "init"
	SessionStatePending   SessionState = "pending"
	SessionStateVerified  SessionState = "verified"
	SessionStateConnected SessionState = "connected"
	SessionStateError     SessionState = "error"
	SessionStateExpired   SessionState = "expired"
)

// Session represents the state of a P2P connection between Alice and Bob.
type Session struct {
	// Known fingerprint of the expected peer, shared out-of-band.
	ExpectedPeerFingerprint string

	OwnKeys        *kbc.OwnKeys
	OwnFingerprint string

	// Populated after receiving peer keys.
	PeerPubKeys *kbc.PeerKeys // "x25519", "mlkem"

	// Symmetric session key.
	SEKInbound  []byte
	SEKOutbound []byte

	// QUIC control-channel keys, derived in the SAME handshake as the TCP keys from extra seeds
	// in the initial payload, so the handshake gains no round trip. Each channel/direction has
	// its own key, so the QUIC channel never shares key + nonce space with TCP. Empty when the
	// peer is an older build that sent no QUIC seeds; then the QUIC channel is not brought up.
	SEKOutboundQUIC []byte
	SEKInboundQUIC  []byte

	// Negotiated cipher suite for this session.
	CipherMu    sync.Mutex
	CipherSuite kbc.CipherSuite

	// Peer-to-peer TCP connections.
	Session  *SessionSockets
	PeerPort int

	DefaultOutboundPort int
	DefaultInboundPort  int

	GRPCListener net.Listener
	GRPCClient   bindings.KeibiServiceClient
	// ExtraFoldConns, when set, returns additional live SecureConns (the QUIC control lane) to
	// include in a fold round. stageFoldBothConns calls it at staging time on BOTH roles, so
	// every lane that exists then gets the fold's fresh entropy, not just the TCP pair. A QUIC
	// wire that comes up later is covered by the round its own arrival triggers. Must be safe to
	// call from any goroutine; nil entries are skipped.
	ExtraFoldConns func() []*SecureConn

	// Session state and lifecycle
	State       SessionState
	Established time.Time

	Err error

	// Persistent identity flags (learned during handshake).
	OwnIsPersistent  bool
	PeerIsPersistent bool

	// PeerSupportsKeyUpdate is learned from the peer's handshake: true if it advertised the
	// in-band key-update capability. Gates whether the ratchet turns on.
	PeerSupportsKeyUpdate bool

	// Internal timeout deadline
	Deadline time.Time

	// Re-keying state. Rotation is performed by an idle re-handshake, not an in-band key swap,
	// so these only track the last rotation for gating.
	RekeyMu      sync.Mutex
	LastRekeyAt  time.Time
	CurrentEpoch uint64

	// Pending-fold custody. Rounds stage per-conn, but a conn installed AFTER a round (the
	// reconnect handshake swaps sockets mid-round) would miss it while the peer's writer stays
	// armed, making its first folded bump undecryptable. The session remembers un-retired
	// rounds so every install adopts them; a reader commit retires session-wide.
	pendingFoldMu sync.Mutex
	pendingFolds  [][]byte

	// socketsMu guards the Session pointer and its Inbound/Outbound conn pointers against the
	// reconnect handshake installing fresh conns while a detached eager-fold goroutine reads
	// them (same *Session, no other shared lock). A LEAF lock: accessors snapshot a pointer
	// and release before touching the conn, so it never nests with keyMu/wMu/foldMu. All
	// Session.Inbound/Outbound access outside the handshake-private construction goes through
	// the accessors below.
	socketsMu sync.RWMutex

	logger *slog.Logger
}

func InitSession(logger *slog.Logger, defaultOutboundPort int, defaultInboundPort int) (*Session, error) {
	logger = logger.With("service", "session")
	kemDecapsulate, kemEncapsulate, err := kbc.GenerateMLKEMKeypair()
	if err != nil {
		logger.Error("Failed to generate session mlkem keys", "error", err)
		return nil, err
	}

	ecdPriv, ecdPub, err := kbc.GenerateX25519Keypair()
	if err != nil {
		logger.Error("Failed to generate session x25519 keys", "error", err)
		return nil, err
	}

	ownKeys := kbc.OwnKeys{
		MlKemPrivate:  kemDecapsulate,
		MlKemPublic:   kemEncapsulate,
		X25519Private: ecdPriv,
		X25519Public:  ecdPub,
	}

	ownFingerprint, err := ownKeys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute fingerprint", "error", err)
		return nil, err
	}

	return &Session{
		logger:              logger,
		OwnKeys:             &ownKeys,
		OwnFingerprint:      ownFingerprint,
		State:               SessionInit,
		DefaultOutboundPort: defaultOutboundPort,
		DefaultInboundPort:  defaultInboundPort,
	}, nil
}

// InitSessionWithKeys creates a session using pre-existing keys (for persistent identity).
// Same as InitSession but skips key generation.
func InitSessionWithKeys(logger *slog.Logger, keys *kbc.OwnKeys, defaultOutboundPort int, defaultInboundPort int) (*Session, error) {
	logger = logger.With("service", "session")
	if err := keys.Validate(); err != nil {
		return nil, fmt.Errorf("invalid keys: %w", err)
	}

	ownFingerprint, err := keys.Fingerprint()
	if err != nil {
		logger.Error("Failed to compute fingerprint", "error", err)
		return nil, err
	}

	return &Session{
		logger:              logger,
		OwnKeys:             keys,
		OwnFingerprint:      ownFingerprint,
		State:               SessionInit,
		DefaultOutboundPort: defaultOutboundPort,
		DefaultInboundPort:  defaultInboundPort,
		OwnIsPersistent:     true,
	}, nil
}

func (s *Session) GetFingerPrint() string {
	return s.OwnFingerprint
}

// NegotiatedSuite returns the cipher suite chosen during the handshake, taken under the
// cipher lock, defaulting to the first supported suite when unset. The QUIC control
// channel uses the same suite as the TCP channel.
func (s *Session) NegotiatedSuite() kbc.CipherSuite {
	s.CipherMu.Lock()
	defer s.CipherMu.Unlock()
	if s.CipherSuite == "" {
		return kbc.SupportedCiphers()[0]
	}
	return s.CipherSuite
}

// ResetOutboundCrypto clears the outbound shared key and negotiated cipher suite so a fresh
// outbound handshake can run (e.g. when falling back to the bridge). The caller closes any
// existing outbound conn.
func (s *Session) ResetOutboundCrypto() {
	s.SEKOutbound = nil
	s.CipherMu.Lock()
	s.CipherSuite = ""
	s.CipherMu.Unlock()
}

// ResetInboundCrypto clears the inbound shared key. The caller closes any existing inbound
// conn.
func (s *Session) ResetInboundCrypto() {
	s.SEKInbound = nil
}

// SessionSockets holds a duplex connection for peer communication. The two ends are
// built by the handshake with opposite nonce prefixes (Inbound uses NoncePrefixInbound,
// Outbound uses NoncePrefixOutbound) so a socket's shared key never reuses a nonce.
// Access the pointers through the Session accessors (InboundConn/OutboundConn/BothConns/
// Set*), never directly, so the socketsMu guard holds.
type SessionSockets struct {
	Inbound  *SecureConn // Bob -> Alice
	Outbound *SecureConn // Alice -> Bob
}

// InboundConn returns the current inbound conn under socketsMu, or nil. The pointer is
// snapshotted and the lock released before the caller uses it (leaf-lock discipline).
func (s *Session) InboundConn() *SecureConn {
	s.socketsMu.RLock()
	defer s.socketsMu.RUnlock()
	if s.Session == nil {
		return nil
	}
	return s.Session.Inbound
}

// OutboundConn returns the current outbound conn under socketsMu, or nil.
func (s *Session) OutboundConn() *SecureConn {
	s.socketsMu.RLock()
	defer s.socketsMu.RUnlock()
	if s.Session == nil {
		return nil
	}
	return s.Session.Outbound
}

// BothConns snapshots both directions under one RLock, so a caller that needs a consistent
// pair (a fold round staging both) never mixes a pre- and post-reconnect conn.
func (s *Session) BothConns() (inbound, outbound *SecureConn) {
	s.socketsMu.RLock()
	defer s.socketsMu.RUnlock()
	if s.Session == nil {
		return nil, nil
	}
	return s.Session.Inbound, s.Session.Outbound
}

// SetInboundConn assigns the inbound conn under socketsMu (nil on teardown). Creates the
// SessionSockets holder on first use.
func (s *Session) SetInboundConn(c *SecureConn) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()
	if s.Session == nil {
		s.Session = &SessionSockets{}
	}
	s.Session.Inbound = c
}

// SetOutboundConn assigns the outbound conn under socketsMu (nil on teardown).
func (s *Session) SetOutboundConn(c *SecureConn) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()
	if s.Session == nil {
		s.Session = &SessionSockets{}
	}
	s.Session.Outbound = c
}

// NewSession initializes a new session with a timeout deadline.
func NewSession(logger *slog.Logger, expectedFingerprint string, timeout time.Duration) *Session {
	return &Session{
		ExpectedPeerFingerprint: expectedFingerprint,
		State:                   SessionStatePending,
		Established:             time.Now(),
		Deadline:                time.Now().Add(timeout),
		logger:                  logger,
	}
}

// IsExpired returns true if the session has passed its allowed timeout.
func (s *Session) IsExpired() bool {
	return time.Now().After(s.Deadline)
}

// MarkError marks the session as errored and closes any open connections.
func (s *Session) MarkError(err error) {
	// Error path; a failure here is moot.
	_ = s.Transition(SessionStateError) // errors ignored to prevent panic
	s.Err = err

	in, out := s.BothConns()
	if in != nil {
		_ = in.Close()
	}
	if out != nil {
		_ = out.Close()
	}
}

// IsVerified returns true if the fingerprint matched and session is accepted.
func (s *Session) IsVerified() bool {
	return s.State == SessionStateVerified || s.State == SessionStateConnected
}

// ReadyForEncryption returns true when both connections and SEKs are set.
func (s *Session) ReadyForEncryption() bool {
	in, out := s.BothConns()
	return s.State == SessionStateConnected &&
		s.SEKInbound != nil &&
		s.SEKOutbound != nil &&
		in != nil &&
		out != nil
}

// keyUpdateDisabled gates whether this peer advertises the in-band key-update capability.
// Default enabled (zero value false). It is the single source of truth for the capability.
var keyUpdateDisabled atomic.Bool

// SetKeyUpdateEnabled toggles the in-band key-update capability advertised on new handshakes.
// Off makes this peer negotiate the re-handshake fallback instead. Affects only handshakes
// after the call, not an established session.
func SetKeyUpdateEnabled(on bool) { keyUpdateDisabled.Store(!on) }

// ownSupportsKeyUpdate reports this peer's advertised key-update capability, honouring the
// SetKeyUpdateEnabled off-switch.
func ownSupportsKeyUpdate() bool { return !keyUpdateDisabled.Load() }

// UseKeyUpdate reports whether the in-band ratchet may run: only when both peers advertised
// the capability. A new-to-old pair, or one with the off-switch set, returns false and
// rotates via the re-handshake fallback.
func (s *Session) UseKeyUpdate() bool {
	return ownSupportsKeyUpdate() && s.PeerSupportsKeyUpdate
}

// maxWriterEpoch returns the higher in-band ratchet epoch across both directions, or 0 if no
// conns exist. It is the epoch that climbs toward the wrap guard as the ratchet rotates.
func (s *Session) maxWriterEpoch() uint16 {
	in, out := s.BothConns()
	var e uint16
	if in != nil {
		e = in.WriterEpoch()
	}
	if out != nil {
		if oe := out.WriterEpoch(); oe > e {
			e = oe
		}
	}
	return e
}

// NearEpochWrap reports whether a key-update session's writer epoch has climbed close enough to
// the wrap guard that it must re-handshake for a fresh epoch-0 key. Gates on UseKeyUpdate (only
// key-update sessions reach the guard); without it the ratchet would pin its epoch at the guard
// and stop advancing forward secrecy.
func (s *Session) NearEpochWrap() bool {
	return s.UseKeyUpdate() && s.maxWriterEpoch() >= epochRehandshakeThreshold
}

// ApplyKeyUpdateNegotiation turns the ratchet on (or off) on both live conns to match the
// negotiated capability. Must run after both handshakes complete, so PeerSupportsKeyUpdate is
// known, and before the gRPC reader goroutine starts, since SetKeyUpdate is not safe to flip
// under a live reader.
func (s *Session) ApplyKeyUpdateNegotiation() {
	in, out := s.BothConns()
	on := s.UseKeyUpdate()
	if !on && s.logger != nil {
		// Post-0.4 every peer supports the in-band ratchet, so a ratchet-less session means an
		// old peer or a downgrade: keys then rotate only via the re-handshake fallback. Loud on
		// purpose, a silent epoch-0 session must not pass unnoticed.
		s.logger.Warn("Session running WITHOUT in-band rekey (peer did not negotiate key-update); forward secrecy limited to re-handshake rotations",
			"own", ownSupportsKeyUpdate(), "peer", s.PeerSupportsKeyUpdate)
	}
	if in != nil {
		in.SetKeyUpdate(on)
	}
	if out != nil {
		out.SetKeyUpdate(on)
	}
}

// ========== RE-KEYING ==========

// ShouldRekey returns true if either connection has exceeded the rekey threshold.
func (s *Session) ShouldRekey() bool {
	in, out := s.BothConns()
	if in != nil && in.ShouldRekey() {
		return true
	}
	if out != nil && out.ShouldRekey() {
		return true
	}
	return false
}

// The in-band rekey handlers (HandleRekeyRequest/HandleRekeyResponse) were removed: swapping a
// live SecureConn key mid-stream with no on-wire key epoch was unsafe, since a stray, replayed,
// or forged Rekey RPC from the untrusted relay could brick the connection. Rotation is done out
// of band by an idle re-handshake instead.

// GetRekeyEpoch returns the current key epoch.
func (s *Session) GetRekeyEpoch() uint64 {
	s.RekeyMu.Lock()
	defer s.RekeyMu.Unlock()
	return s.CurrentEpoch
}

// rememberPendingFold keeps a session-scope copy of an un-retired fold secret so a conn
// installed after this round still adopts it. Same cap as the reader set.
func (s *Session) rememberPendingFold(secret []byte) {
	s.pendingFoldMu.Lock()
	defer s.pendingFoldMu.Unlock()
	s.pendingFolds = append(s.pendingFolds, append([]byte(nil), secret...))
	if len(s.pendingFolds) > maxStagedFolds {
		zeroize(s.pendingFolds[0])
		s.pendingFolds = s.pendingFolds[1:]
	}
}

// retirePendingFolds drops rounds STRICTLY OLDER than the committed one and keeps the
// committed round itself: a later lane may still re-handshake and need it (the peer's
// writer stays armed with it until that lane also commits). Writers fold generations
// monotonically, so nothing below the committed round can arrive again. A newer round's
// commit, or the cap, eventually drops it.
func (s *Session) retirePendingFolds(committed []byte) {
	s.pendingFoldMu.Lock()
	defer s.pendingFoldMu.Unlock()
	idx := -1
	for i := range s.pendingFolds {
		if bytes.Equal(s.pendingFolds[i], committed) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	for i := 0; i < idx; i++ {
		zeroize(s.pendingFolds[i])
	}
	s.pendingFolds = append(s.pendingFolds[:0], s.pendingFolds[idx:]...)
}

// adoptPendingFolds stages every remembered round on a freshly installed conn's reader,
// oldest first so the try order matches the peer writer's monotonic folds. Deep-copies
// under the lock and releases before staging, so it never holds pendingFoldMu across
// keyMu/foldMu (breaks the latent pendingFoldMu<->keyMu cycle) and the copies cannot race
// retirePendingFolds' zeroize.
func (s *Session) adoptPendingFolds(c *SecureConn) {
	s.pendingFoldMu.Lock()
	snap := make([][]byte, len(s.pendingFolds))
	for i, sec := range s.pendingFolds {
		snap[i] = append([]byte(nil), sec...)
	}
	s.pendingFoldMu.Unlock()
	for _, sec := range snap {
		c.stageReaderFold(sec)
	}
}

// AdoptFoldCustody makes a conn that joins the session AFTER a fold round adopt every
// un-retired round and retire session-wide when it commits one. Both the TCP install path
// and the QUIC control lane (whose conns are created outside install*) call it, so no lane
// can miss a round the peer's writer stays armed with. Safe on any conn before it carries
// a live reader.
func (s *Session) AdoptFoldCustody(c *SecureConn) {
	if c == nil {
		return
	}
	s.adoptPendingFolds(c)
	c.setFoldCommitHook(s.retirePendingFolds)
}

// installInbound and installOutbound are the one path that mounts a SecureConn into the
// session: assign first (publish the conn), then adopt pending fold rounds and wire the
// commit-retirement hook. Assign-before-adopt is load-bearing: paired with
// remember-before-snapshot in stageFoldBothConns, it gives every stage/install interleaving
// a happens-before edge that lands round R on the fresh conn (direct stage or adopt). The
// conn has no live reader until the handshake returns, so publishing it before adoption
// exposes nothing.
func (s *Session) installInbound(c *SecureConn) {
	s.SetInboundConn(c) // publish under socketsMu
	s.AdoptFoldCustody(c)
}

func (s *Session) installOutbound(c *SecureConn) {
	s.SetOutboundConn(c) // publish under socketsMu
	s.AdoptFoldCustody(c)
}
