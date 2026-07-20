// ABOUTME: Ephemeral hybrid-KEM fold: a fresh ML-KEM-1024 + X25519 exchange whose shared
// ABOUTME: secret is mixed into the session ratchet for post-quantum forward secrecy.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/sha512"
	"errors"
	"fmt"
)

// labelFoldCombine domain-separates the fold combiner from every other KDF use.
const labelFoldCombine = "KeibiDrop-fold-combine-v1"

// minFoldSaltSize is the smallest accepted fold salt. The caller must pass a session-
// unique value bound to the authenticated handshake (an SEK-derived value or the
// handshake-transcript hash); this guard fails closed on an empty or stub salt.
const minFoldSaltSize = 32

// combineFold derives the 32-byte fold secret from the classical (X25519) and post-
// quantum (ML-KEM) shared secrets, salted with a session-bound value. The order is fixed
// (classical then post-quantum) so both peers agree, and the distinct label keeps the
// result separate from every other derivation.
func combineFold(x25519Shared, mlkemShared, salt []byte) ([]byte, error) {
	if len(salt) < minFoldSaltSize {
		return nil, fmt.Errorf("fold salt must be at least %d bytes, got %d", minFoldSaltSize, len(salt))
	}
	return deriveKeyInternalWithSalt(sha512.New, salt, labelFoldCombine, KeySize, x25519Shared, mlkemShared)
}

// FoldInitiator holds one fold round's ephemeral private keys for the initiator (the
// deterministic lower-fingerprint peer). It must live only for the round: create it, send
// the publics, call Derive with the response, and let it fall out of scope. Never store it
// on a long-lived struct; its ephemeral private keys are what forward secrecy rests on.
type FoldInitiator struct {
	mlkemPriv  *mlkem.DecapsulationKey1024
	x25519Priv *ecdh.PrivateKey
	mlkemPub   []byte
	x25519Pub  []byte
}

// NewFoldInitiator generates the initiator's fresh ephemeral ML-KEM-1024 and X25519
// keypairs for one fold round.
func NewFoldInitiator() (*FoldInitiator, error) {
	mlkemPriv, mlkemPub, err := GenerateMLKEMKeypair()
	if err != nil {
		return nil, fmt.Errorf("fold: ml-kem keygen: %w", err)
	}
	x25519Priv, x25519Pub, err := GenerateX25519Keypair()
	if err != nil {
		return nil, fmt.Errorf("fold: x25519 keygen: %w", err)
	}
	return &FoldInitiator{
		mlkemPriv:  mlkemPriv,
		x25519Priv: x25519Priv,
		mlkemPub:   mlkemPub.Bytes(),
		x25519Pub:  x25519Pub.Bytes(),
	}, nil
}

// MLKEMPublic returns the initiator's ephemeral ML-KEM public to send to the responder.
func (fi *FoldInitiator) MLKEMPublic() []byte { return fi.mlkemPub }

// X25519Public returns the initiator's ephemeral X25519 public to send to the responder.
func (fi *FoldInitiator) X25519Public() []byte { return fi.x25519Pub }

// Derive completes the initiator's side: it decapsulates the responder's ML-KEM
// ciphertext, does the X25519 ECDH with the responder's ephemeral public, and combines
// them (salted) into the 32-byte fold secret. The responder's X25519 public is validated
// through the standard constructor, so a malformed value fails the round. The ephemeral
// private keys are dropped before returning, best-effort: Go exposes no wipe for these
// types, so the real control is this single-round lifetime.
func (fi *FoldInitiator) Derive(mlkemCiphertext, respX25519Pub, salt []byte) ([]byte, error) {
	if fi.mlkemPriv == nil || fi.x25519Priv == nil {
		return nil, errors.New("fold: initiator already consumed")
	}
	defer fi.drop()

	respPub, err := ecdh.X25519().NewPublicKey(respX25519Pub)
	if err != nil {
		return nil, fmt.Errorf("fold: bad responder x25519 public: %w", err)
	}
	mlkemShared, err := fi.mlkemPriv.Decapsulate(mlkemCiphertext)
	if err != nil {
		return nil, fmt.Errorf("fold: ml-kem decapsulate: %w", err)
	}
	x25519Shared, err := fi.x25519Priv.ECDH(respPub)
	if err != nil {
		return nil, fmt.Errorf("fold: x25519 ecdh: %w", err)
	}
	return combineFold(x25519Shared, mlkemShared, salt)
}

// drop releases the ephemeral private keys. Go's crypto/mlkem and crypto/ecdh private keys
// expose no wipe, so this only unreferences them; the real control is the single-round
// lifetime enforced by the caller.
func (fi *FoldInitiator) drop() {
	fi.mlkemPriv = nil
	fi.x25519Priv = nil
}

// EphemeralFoldRespond completes the responder's side of a fold round. Given the
// initiator's ephemeral ML-KEM and X25519 publics, it generates its own ephemeral X25519
// keypair, encapsulates to the initiator's ML-KEM public, does the X25519 ECDH, and
// combines (salted) into the 32-byte fold secret. It returns the fold secret, the ML-KEM
// ciphertext, and the responder's ephemeral X25519 public for the initiator to finish
// with. The initiator's publics are validated through the standard constructors; the
// responder's ephemeral private falls out of scope on return.
func EphemeralFoldRespond(initMLKEMPub, initX25519Pub, salt []byte) (foldSecret, mlkemCiphertext, respX25519Pub []byte, err error) {
	// Fail fast on a short/absent salt, before generating a keypair or encapsulating.
	if len(salt) < minFoldSaltSize {
		return nil, nil, nil, fmt.Errorf("fold salt must be at least %d bytes, got %d", minFoldSaltSize, len(salt))
	}
	encapKey, err := mlkem.NewEncapsulationKey1024(initMLKEMPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fold: bad initiator ml-kem public: %w", err)
	}
	initXPub, err := ecdh.X25519().NewPublicKey(initX25519Pub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fold: bad initiator x25519 public: %w", err)
	}
	respPriv, respPub, err := GenerateX25519Keypair()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fold: x25519 keygen: %w", err)
	}

	mlkemShared, ct := encapKey.Encapsulate()
	x25519Shared, err := respPriv.ECDH(initXPub)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("fold: x25519 ecdh: %w", err)
	}
	foldSecret, err = combineFold(x25519Shared, mlkemShared, salt)
	if err != nil {
		return nil, nil, nil, err
	}
	return foldSecret, ct, respPub.Bytes(), nil
}
