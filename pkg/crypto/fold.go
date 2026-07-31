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
// unique value bound to the authenticated handshake; this fails closed on an empty salt.
const minFoldSaltSize = 32

// combineFold derives the 32-byte fold secret from the X25519 and ML-KEM shared secrets
// with a session-bound salt. Order is fixed (classical then post-quantum) so peers agree.
func combineFold(x25519Shared, mlkemShared, salt []byte) ([]byte, error) {
	if len(salt) < minFoldSaltSize {
		return nil, fmt.Errorf("fold salt must be at least %d bytes, got %d", minFoldSaltSize, len(salt))
	}
	return deriveKeyInternalWithSalt(sha512.New, salt, labelFoldCombine, KeySize, x25519Shared, mlkemShared)
}

// FoldInitiator holds one fold round's ephemeral private keys for the initiator (the
// lower-fingerprint peer). Single-round lifetime: forward secrecy rests on these ephemeral
// keys not outliving the round.
type FoldInitiator struct {
	mlkemPriv  *mlkem.DecapsulationKey1024
	x25519Priv *ecdh.PrivateKey
	mlkemPub   []byte
	x25519Pub  []byte
}

// NewFoldInitiator generates the initiator's ephemeral ML-KEM-1024 and X25519 keypairs.
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

// Derive completes the initiator's side: decapsulate the responder's ML-KEM ciphertext,
// X25519 ECDH with its ephemeral public, combine (salted) into the 32-byte fold secret.
// The constructor validates the responder public. Keys drop on return, best-effort;
// Go has no wipe for these types, so single-round lifetime is the real control.
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

// drop unreferences the ephemeral private keys. Go's mlkem/ecdh keys expose no wipe, so
// the real control is the caller's single-round lifetime.
func (fi *FoldInitiator) drop() {
	fi.mlkemPriv = nil
	fi.x25519Priv = nil
}

// EphemeralFoldRespond completes the responder's side: generate an ephemeral X25519 keypair,
// encapsulate to the initiator's ML-KEM public, X25519 ECDH, combine (salted) into the
// 32-byte fold secret. Returns the fold secret, ML-KEM ciphertext, and responder X25519
// public for the initiator to finish. The constructors validate the initiator publics.
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
