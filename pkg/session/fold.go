// ABOUTME: Session-level entropy fold: derives the session-bound fold salt and drives the
// ABOUTME: ephemeral hybrid-KEM exchange over the Rekey RPC, staging the secret on both conns.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// EncSeeds map keys for the fold exchange. The request carries the initiator's ephemeral
// publics; the response carries the responder's ML-KEM ciphertext and ephemeral X25519 public.
// "x25519" is reused across both directions on purpose (it names the field, not a direction).
const (
	foldKeyMLKEM      = "mlkem"
	foldKeyX25519     = "x25519"
	foldKeyCiphertext = "ct"
)

// ErrFoldNotReady is returned when a fold is attempted on a session whose keys or conns are
// not established, or whose in-band ratchet was not negotiated. Staging a fold on such a
// session would never apply, so the fold fails closed instead of silently doing nothing.
var ErrFoldNotReady = errors.New("session not ready for entropy fold")

// ErrFoldNoClient is returned by InitiateFold when the session has no gRPC client to carry
// the fold exchange to the peer.
var ErrFoldNoClient = errors.New("no grpc client to initiate entropy fold")

// ErrFoldWrongRole is returned when RespondToFold is called on the deterministic fold
// initiator. Only the responder (higher fingerprint) answers a fold; the initiator drives one.
// Refusing out of role stops a misbehaving peer from racing a second, conflicting secret that
// would reset the conn.
var ErrFoldWrongRole = errors.New("entropy fold: this peer initiates, it does not respond")

// FoldSalt derives the 32-byte, session-bound salt both peers must agree on for a fold round.
// The two SEKs are the same pair on both peers, labeled oppositely (peer A's SEKOutbound equals
// peer B's SEKInbound), so they are sorted before concatenation: without that normalization the
// peers would derive different fold secrets and every fold would fail.
func (s *Session) FoldSalt() ([]byte, error) {
	if len(s.SEKInbound) == 0 || len(s.SEKOutbound) == 0 {
		return nil, fmt.Errorf("fold salt: %w: session keys not set", ErrFoldNotReady)
	}
	lo, hi := s.SEKInbound, s.SEKOutbound
	if bytes.Compare(lo, hi) > 0 {
		lo, hi = hi, lo
	}
	ikm := make([]byte, 0, len(lo)+len(hi))
	ikm = append(ikm, lo...)
	ikm = append(ikm, hi...)
	return kbc.DeriveFoldSalt(ikm)
}

// IsFoldInitiator reports whether this peer drives the fold. The lexicographically lower
// fingerprint initiates, the same deterministic election ReconnectManager uses, so exactly
// one peer calls InitiateFold while the other answers via RespondToFold.
func (s *Session) IsFoldInitiator() bool {
	return s.OwnFingerprint < s.ExpectedPeerFingerprint
}

// foldReady reports whether a fold can be staged: both conns exist and the in-band ratchet
// was negotiated (a fold applies only through the ratchet, so without it staging is a no-op).
func (s *Session) foldReady() bool {
	in, out := s.BothConns()
	return in != nil && out != nil && s.UseKeyUpdate()
}

// stageFoldBothConns stages the fold secret on both directions of both conns. isInitiator
// selects the writer gate: an initiator's writers fold promptly, a responder's wait until its
// own reader commits the initiator's folded frame. Every reader is staged before any writer is
// armed, so no writer folds before both conns can follow the peer's fold-back.
func (s *Session) stageFoldBothConns(secret []byte, isInitiator bool) {
	// Remember BEFORE the conns snapshot: this orders custody ahead of the socketsMu-guarded
	// read of the conns, so a conn being installed concurrently (reconnect swap) either lands
	// in this snapshot (its Set*Conn happened-before BothConns -> staged directly) or adopts
	// the round at install (its adopt runs after this remember). Paired with the socketsMu
	// publish in install*, every interleaving lands the round on the fresh conn.
	s.rememberPendingFold(secret)
	conns := make([]*SecureConn, 0, 4)
	in, out := s.BothConns()
	for _, c := range []*SecureConn{in, out} {
		if c != nil {
			conns = append(conns, c)
		}
	}
	// The QUIC control lane folds in the same round: same secret, same per-conn staging
	// machinery, no wire change. ExtraFoldConns snapshots whichever QUIC conns exist right
	// now; a wire that appears later is covered by the round its arrival triggers.
	if s.ExtraFoldConns != nil {
		for _, c := range s.ExtraFoldConns() {
			if c != nil {
				conns = append(conns, c)
			}
		}
	}
	// Every reader staged before any writer is armed: the cross-conn invariant, now spanning the
	// extra conns too.
	for _, c := range conns {
		c.stageReaderFold(secret)
		c.setFoldCommitHook(s.retirePendingFolds)
	}
	for _, c := range conns {
		c.armWriterFold(secret, isInitiator)
	}
}

// RespondToFold is the responder side of a fold round (the gRPC server handler). It derives the
// session-bound salt, completes the hybrid-KEM exchange against the initiator's publics, stages
// the secret on both conns as a gated responder, and returns the ML-KEM ciphertext and its
// ephemeral X25519 public for the initiator to finish with. Fails closed if the session is not
// ready or the request is malformed, so a stray or forged Rekey RPC cannot drive a fold.
func (s *Session) RespondToFold(req *bindings.RekeyRequest) (*bindings.RekeyResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("fold respond: %w: nil request", ErrFoldNotReady)
	}
	if s.IsFoldInitiator() {
		// The designated initiator never answers a fold; it drives one. Refusing out of role
		// stops a misbehaving peer from racing a second secret into our staged fold state.
		return nil, ErrFoldWrongRole
	}
	if !s.foldReady() {
		return nil, ErrFoldNotReady
	}
	salt, err := s.FoldSalt()
	if err != nil {
		return nil, err
	}

	initMLKEM := req.EncSeeds[foldKeyMLKEM]
	initX25519 := req.EncSeeds[foldKeyX25519]
	if len(initMLKEM) == 0 || len(initX25519) == 0 {
		return nil, fmt.Errorf("fold respond: request missing %q or %q public", foldKeyMLKEM, foldKeyX25519)
	}

	secret, ct, respX25519, err := kbc.EphemeralFoldRespond(initMLKEM, initX25519, salt)
	if err != nil {
		return nil, fmt.Errorf("fold respond: %w", err)
	}
	s.stageFoldBothConns(secret, false)
	zeroize(secret)

	// Both peers surface a fold, not just the initiator. Guarded: RespondToFold is reachable
	// from the gRPC handler and external-package tests that build a bare Session with no logger.
	if s.logger != nil {
		s.logger.With("event", "fold").Info("Entropy fold committed (responder); session is forward-secret against later identity-key theft",
			"epoch", s.maxWriterEpoch(), "dir", "respond")
	}

	return &bindings.RekeyResponse{
		EncSeeds: map[string][]byte{
			foldKeyCiphertext: ct,
			foldKeyX25519:     respX25519,
		},
		Epoch: req.Epoch,
	}, nil
}

// InitiateFold is the initiator side of a fold round over the session's own TCP gRPC
// client. See InitiateFoldVia for the round itself.
func (s *Session) InitiateFold(ctx context.Context) error {
	if s.GRPCClient == nil {
		return ErrFoldNoClient
	}
	return s.InitiateFoldVia(ctx, s.GRPCClient)
}

// InitiateFoldVia runs the initiator side of a fold round over the given KeibiService client;
// the caller picks the lane (the eager-fold driver tries QUIC first, falls back to TCP), and the
// round is identical either way. It derives the salt, generates a fresh ephemeral keypair, sends
// its publics over the Rekey RPC, derives the fold secret from the response, and stages it on
// both conns as the initiator. Lane FAILOVER is safe: a failed attempt that still staged the
// responder is superseded by the retry's fresh round (staging is supersede-idempotent). Lane
// DUPLICATION of one round would NOT be safe: KEM encapsulation is randomized, so a duplicate
// request derives a different secret.
func (s *Session) InitiateFoldVia(ctx context.Context, client bindings.KeibiServiceClient) error {
	if client == nil {
		return ErrFoldNoClient
	}
	if !s.foldReady() {
		return ErrFoldNotReady
	}
	salt, err := s.FoldSalt()
	if err != nil {
		return err
	}

	initiator, err := kbc.NewFoldInitiator()
	if err != nil {
		return fmt.Errorf("fold initiate: %w", err)
	}
	req := &bindings.RekeyRequest{EncSeeds: map[string][]byte{
		foldKeyMLKEM:  initiator.MLKEMPublic(),
		foldKeyX25519: initiator.X25519Public(),
	}}

	resp, err := client.Rekey(ctx, req)
	if err != nil {
		return fmt.Errorf("fold initiate: rekey rpc: %w", err)
	}
	if resp == nil {
		return fmt.Errorf("fold initiate: empty rekey response")
	}
	ct := resp.EncSeeds[foldKeyCiphertext]
	respX25519 := resp.EncSeeds[foldKeyX25519]
	if len(ct) == 0 || len(respX25519) == 0 {
		return fmt.Errorf("fold initiate: response missing %q or %q", foldKeyCiphertext, foldKeyX25519)
	}

	secret, err := initiator.Derive(ct, respX25519, salt)
	if err != nil {
		return fmt.Errorf("fold initiate: derive: %w", err)
	}
	s.stageFoldBothConns(secret, true)
	zeroize(secret)
	return nil
}
