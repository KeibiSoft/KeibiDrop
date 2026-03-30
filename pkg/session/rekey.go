// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"fmt"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
)

// CreateRekeyRequest generates a RekeyRequest with new encapsulated seeds.
// Returns the request, the derived new key (for outbound), and any error.
func CreateRekeyRequest(ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys, epoch uint64, suite kbc.CipherSuite) (*bindings.RekeyRequest, []byte, error) {
	if ownKeys == nil {
		return nil, nil, fmt.Errorf("own keys is nil")
	}
	if peerKeys == nil {
		return nil, nil, fmt.Errorf("peer keys is nil")
	}
	if err := ownKeys.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid own keys: %w", err)
	}
	if err := peerKeys.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid peer keys: %w", err)
	}

	// Generate new random seed and encapsulate with X25519.
	seed1 := kbc.GenerateSeed()
	encSeed1, err := kbc.X25519Encapsulate(seed1, ownKeys.X25519Private, peerKeys.X25519Public)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519 encapsulate failed: %w", err)
	}

	// Encapsulate with ML-KEM.
	seed2, encSeed2 := peerKeys.MlKemPublic.Encapsulate()

	// Derive the new key from both seeds using the negotiated cipher suite.
	newKey, err := kbc.DeriveKey(suite, seed1, seed2)
	if err != nil {
		return nil, nil, fmt.Errorf("key derivation failed: %w", err)
	}

	req := &bindings.RekeyRequest{
		EncSeeds: map[string][]byte{
			"x25519": encSeed1,
			"mlkem":  encSeed2,
		},
		Epoch: epoch,
	}

	return req, newKey, nil
}

// ProcessRekeyRequest handles an incoming RekeyRequest.
// Returns a RekeyResponse, the derived new inbound key, and any error.
func ProcessRekeyRequest(req *bindings.RekeyRequest, ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys, suite kbc.CipherSuite) (*bindings.RekeyResponse, []byte, error) {
	if ownKeys == nil {
		return nil, nil, fmt.Errorf("own keys is nil")
	}
	if peerKeys == nil {
		return nil, nil, fmt.Errorf("peer keys is nil")
	}
	if err := ownKeys.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid own keys: %w", err)
	}
	if err := peerKeys.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid peer keys: %w", err)
	}

	encSeed1, ok := req.EncSeeds["x25519"]
	if !ok {
		return nil, nil, fmt.Errorf("missing x25519 encapsulated seed")
	}
	encSeed2, ok := req.EncSeeds["mlkem"]
	if !ok {
		return nil, nil, fmt.Errorf("missing mlkem encapsulated seed")
	}

	// Decapsulate X25519 seed.
	seed1, err := kbc.X25519Decapsulate(encSeed1, ownKeys.X25519Private, peerKeys.X25519Public)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519 decapsulate failed: %w", err)
	}

	// Decapsulate ML-KEM seed.
	seed2, err := ownKeys.MlKemPrivate.Decapsulate(encSeed2)
	if err != nil {
		return nil, nil, fmt.Errorf("mlkem decapsulate failed: %w", err)
	}

	// Derive the new inbound key.
	newInboundKey, err := kbc.DeriveKey(suite, seed1, seed2)
	if err != nil {
		return nil, nil, fmt.Errorf("inbound key derivation failed: %w", err)
	}

	// Create our response with new seeds for the peer's inbound direction.
	respReq, _, err := CreateRekeyRequest(ownKeys, peerKeys, req.Epoch, suite)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create response seeds: %w", err)
	}

	resp := &bindings.RekeyResponse{
		EncSeeds: respReq.EncSeeds,
		Epoch:    req.Epoch,
	}

	return resp, newInboundKey, nil
}

// ProcessRekeyResponse handles an incoming RekeyResponse.
// Returns the derived new outbound key (peer's response seeds).
func ProcessRekeyResponse(resp *bindings.RekeyResponse, ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys, suite kbc.CipherSuite) ([]byte, error) {
	if ownKeys == nil {
		return nil, fmt.Errorf("own keys is nil")
	}
	if peerKeys == nil {
		return nil, fmt.Errorf("peer keys is nil")
	}
	if err := ownKeys.Validate(); err != nil {
		return nil, fmt.Errorf("invalid own keys: %w", err)
	}
	if err := peerKeys.Validate(); err != nil {
		return nil, fmt.Errorf("invalid peer keys: %w", err)
	}

	encSeed1, ok := resp.EncSeeds["x25519"]
	if !ok {
		return nil, fmt.Errorf("missing x25519 encapsulated seed in response")
	}
	encSeed2, ok := resp.EncSeeds["mlkem"]
	if !ok {
		return nil, fmt.Errorf("missing mlkem encapsulated seed in response")
	}

	// Decapsulate X25519 seed.
	seed1, err := kbc.X25519Decapsulate(encSeed1, ownKeys.X25519Private, peerKeys.X25519Public)
	if err != nil {
		return nil, fmt.Errorf("x25519 decapsulate failed: %w", err)
	}

	// Decapsulate ML-KEM seed.
	seed2, err := ownKeys.MlKemPrivate.Decapsulate(encSeed2)
	if err != nil {
		return nil, fmt.Errorf("mlkem decapsulate failed: %w", err)
	}

	// Derive the new key for receiving from peer.
	newKey, err := kbc.DeriveKey(suite, seed1, seed2)
	if err != nil {
		return nil, fmt.Errorf("key derivation failed: %w", err)
	}

	return newKey, nil
}
