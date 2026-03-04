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

// CreateRekeyRequest generates a RekeyRequest with fresh ephemeral keys and encapsulated seeds.
// This ensures Perfect Forward Secrecy (PFS) by not relying solely on static identity keys.
func CreateRekeyRequest(ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys, epoch uint64) (*bindings.RekeyRequest, []byte, error) {
	if ownKeys == nil || peerKeys == nil {
		return nil, nil, fmt.Errorf("keys cannot be nil")
	}

	// 1. Generate ephemeral re-keying keys.
	_, ephMlKemPub, err := kbc.GenerateMLKEMKeypair()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate ephemeral mlkem: %w", err)
	}
	ephXPriv, ephXPub, err := kbc.GenerateX25519Keypair()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate ephemeral x25519: %w", err)
	}

	// 2. Encapsulate seeds against peer's STATIC identity keys.
	// Note: We use ephemeral sender keys for X25519 to provide PFS.
	seed1 := kbc.GenerateSeed()
	encSeed1, err := kbc.X25519Encapsulate(seed1, ephXPriv, peerKeys.X25519Public)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519 encapsulate failed: %w", err)
	}

	seed2, encSeed2 := peerKeys.MlKemPublic.Encapsulate()

	// 3. Derive the new key from both seeds.
	newKey, err := kbc.DeriveChaCha20Key(seed1, seed2)
	if err != nil {
		return nil, nil, fmt.Errorf("key derivation failed: %w", err)
	}

	req := &bindings.RekeyRequest{
		EncSeeds: map[string][]byte{
			"x25519": encSeed1,
			"mlkem":  encSeed2,
		},
		Epoch: epoch,
		EphemeralPublicKeys: map[string][]byte{
			"x25519": ephXPub.Bytes(),
			"mlkem":  ephMlKemPub.Bytes(),
		},
	}

	return req, newKey, nil
}

// ProcessRekeyRequest handles an incoming RekeyRequest.
// Returns a RekeyResponse, the derived new inbound key, and any error.
func ProcessRekeyRequest(req *bindings.RekeyRequest, ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys) (*bindings.RekeyResponse, []byte, error) {
	if ownKeys == nil || peerKeys == nil {
		return nil, nil, fmt.Errorf("keys cannot be nil")
	}

	// 1. Extract peer's ephemeral public keys.
	peerEphXBytes, ok := req.EphemeralPublicKeys["x25519"]
	if !ok {
		return nil, nil, fmt.Errorf("missing peer ephemeral x25519 key")
	}
	peerEphXPub, err := kbc.ParseX25519PublicKey(peerEphXBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse peer ephemeral x25519: %w", err)
	}

	// 2. Decapsulate seeds using our STATIC private keys and peer's EPHEMERAL keys.
	encSeed1, ok := req.EncSeeds["x25519"]
	if !ok {
		return nil, nil, fmt.Errorf("missing x25519 encapsulated seed")
	}
	encSeed2, ok := req.EncSeeds["mlkem"]
	if !ok {
		return nil, nil, fmt.Errorf("missing mlkem encapsulated seed")
	}

	seed1, err := kbc.X25519Decapsulate(encSeed1, ownKeys.X25519Private, peerEphXPub)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519 decapsulate failed: %w", err)
	}

	seed2, err := ownKeys.MlKemPrivate.Decapsulate(encSeed2)
	if err != nil {
		return nil, nil, fmt.Errorf("mlkem decapsulate failed: %w", err)
	}

	// 3. Derive new inbound key.
	newInboundKey, err := kbc.DeriveChaCha20Key(seed1, seed2)
	if err != nil {
		return nil, nil, fmt.Errorf("inbound key derivation failed: %w", err)
	}

	// 4. Create response with our own fresh seeds and ephemeral keys.
	respReq, _, err := CreateRekeyRequest(ownKeys, peerKeys, req.Epoch)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create response: %w", err)
	}

	resp := &bindings.RekeyResponse{
		EncSeeds:            respReq.EncSeeds,
		Epoch:               req.Epoch,
		EphemeralPublicKeys: respReq.EphemeralPublicKeys,
	}

	return resp, newInboundKey, nil
}

// ProcessRekeyResponse handles an incoming RekeyResponse.
func ProcessRekeyResponse(resp *bindings.RekeyResponse, ownKeys *kbc.OwnKeys, peerKeys *kbc.PeerKeys) ([]byte, error) {
	if ownKeys == nil || peerKeys == nil {
		return nil, fmt.Errorf("keys cannot be nil")
	}

	// 1. Extract peer's ephemeral public keys.
	peerEphXBytes, ok := resp.EphemeralPublicKeys["x25519"]
	if !ok {
		return nil, fmt.Errorf("missing peer ephemeral x25519 key in response")
	}
	peerEphXPub, err := kbc.ParseX25519PublicKey(peerEphXBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse peer ephemeral x25519 in response: %w", err)
	}

	// 2. Decapsulate peer's response seeds.
	encSeed1, ok := resp.EncSeeds["x25519"]
	if !ok {
		return nil, fmt.Errorf("missing x25519 seed in response")
	}
	encSeed2, ok := resp.EncSeeds["mlkem"]
	if !ok {
		return nil, fmt.Errorf("missing mlkem seed in response")
	}

	seed1, err := kbc.X25519Decapsulate(encSeed1, ownKeys.X25519Private, peerEphXPub)
	if err != nil {
		return nil, fmt.Errorf("x25519 decapsulate response failed: %w", err)
	}

	seed2, err := ownKeys.MlKemPrivate.Decapsulate(encSeed2)
	if err != nil {
		return nil, fmt.Errorf("mlkem decapsulate response failed: %w", err)
	}

	newKey, err := kbc.DeriveChaCha20Key(seed1, seed2)
	if err != nil {
		return nil, fmt.Errorf("key derivation failed: %w", err)
	}

	return newKey, nil
}
