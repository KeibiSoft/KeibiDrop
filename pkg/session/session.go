// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
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

	// Peer-to-peer TCP connections.
	Session  *SessionSockets
	PeerPort int

	DefaultOutboundPort int
	DefaultInboundPort  int

	GRPCListener net.Listener
	GRPCClient   bindings.KeibiServiceClient

	// Session state and lifecycle
	State       SessionState
	Established time.Time

	Err error

	// Internal timeout deadline
	Deadline time.Time

	// Re-keying state for forward secrecy.
	RekeyMu       sync.Mutex
	LastRekeyAt   time.Time
	CurrentEpoch  uint64
	RekeyPending  bool
	PendingNewKey []byte // awaiting ACK

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

func (s *Session) GetFingerPrint() string {
	return s.OwnFingerprint
}

// SessionSockets holds a duplex connection for peer communication.
type SessionSockets struct {
	Inbound  *SecureConn // Bob -> Alice
	Outbound *SecureConn // Alice -> Bob
}

func NewSessionSockets(connIn, connOut net.Conn, sekIn, sekOut []byte) *SessionSockets {
	return &SessionSockets{
		Inbound:  NewSecureConn(connIn, sekIn),
		Outbound: NewSecureConn(connOut, sekOut),
	}
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
	// We're in an error path; failing here is pointless. If you want strict handling, log it instead.
	_ = s.Transition(SessionStateError) // errors ignored to prevent panic
	s.Err = err

	if s.Session != nil {
		if s.Session.Inbound != nil {
			_ = s.Session.Inbound.Close()
		}
		if s.Session.Outbound != nil {
			_ = s.Session.Outbound.Close()
		}
	}
}

// IsVerified returns true if the fingerprint matched and session is accepted.
func (s *Session) IsVerified() bool {
	return s.State == SessionStateVerified || s.State == SessionStateConnected
}

// ReadyForEncryption returns true when both connections and SEKs are set.
func (s *Session) ReadyForEncryption() bool {
	return s.State == SessionStateConnected &&
		s.SEKInbound != nil &&
		s.SEKOutbound != nil &&
		s.Session != nil &&
		s.Session.Inbound != nil &&
		s.Session.Outbound != nil
}

// ========== RE-KEYING ==========

// ShouldRekey returns true if either connection has exceeded the rekey threshold.
func (s *Session) ShouldRekey() bool {
	if s.Session == nil {
		return false
	}
	if s.Session.Inbound != nil && s.Session.Inbound.ShouldRekey() {
		return true
	}
	if s.Session.Outbound != nil && s.Session.Outbound.ShouldRekey() {
		return true
	}
	return false
}

// HandleRekeyRequest processes an incoming RekeyRequest from peer.
// Updates inbound key and returns response for peer.
func (s *Session) HandleRekeyRequest(req *bindings.RekeyRequest) (*bindings.RekeyResponse, error) {
	s.RekeyMu.Lock()
	defer s.RekeyMu.Unlock()

	if s.OwnKeys == nil || s.PeerPubKeys == nil {
		return nil, fmt.Errorf("session keys not initialized")
	}

	resp, newInboundKey, err := ProcessRekeyRequest(req, s.OwnKeys, s.PeerPubKeys)
	if err != nil {
		return nil, fmt.Errorf("process rekey request: %w", err)
	}

	// Update inbound key immediately.
	s.SEKInbound = newInboundKey
	if s.Session != nil && s.Session.Inbound != nil {
		s.Session.Inbound.UpdateKey(newInboundKey)
	}

	s.CurrentEpoch = req.Epoch
	s.LastRekeyAt = time.Now()
	s.logger.Info("Rekey request processed", "epoch", req.Epoch)

	return resp, nil
}

// HandleRekeyResponse processes an incoming RekeyResponse from peer.
// Updates outbound key after peer acknowledged our rekey request.
func (s *Session) HandleRekeyResponse(resp *bindings.RekeyResponse) error {
	s.RekeyMu.Lock()
	defer s.RekeyMu.Unlock()

	if !s.RekeyPending {
		return fmt.Errorf("unexpected rekey response, no request pending")
	}

	if s.OwnKeys == nil || s.PeerPubKeys == nil {
		return fmt.Errorf("session keys not initialized")
	}

	// Process peer's seeds for our inbound direction.
	newInboundKey, err := ProcessRekeyResponse(resp, s.OwnKeys, s.PeerPubKeys)
	if err != nil {
		return fmt.Errorf("process rekey response: %w", err)
	}

	// Activate pending outbound key.
	if s.PendingNewKey != nil {
		s.SEKOutbound = s.PendingNewKey
		if s.Session != nil && s.Session.Outbound != nil {
			s.Session.Outbound.UpdateKey(s.PendingNewKey)
		}
		s.PendingNewKey = nil
	}

	// Update inbound with peer's new key.
	s.SEKInbound = newInboundKey
	if s.Session != nil && s.Session.Inbound != nil {
		s.Session.Inbound.UpdateKey(newInboundKey)
	}

	s.RekeyPending = false
	s.CurrentEpoch = resp.Epoch
	s.LastRekeyAt = time.Now()
	s.logger.Info("Rekey completed", "epoch", resp.Epoch)

	return nil
}

// GetRekeyEpoch returns the current key epoch.
func (s *Session) GetRekeyEpoch() uint64 {
	s.RekeyMu.Lock()
	defer s.RekeyMu.Unlock()
	return s.CurrentEpoch
}
