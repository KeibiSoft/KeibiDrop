// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package crypto

import (
	"crypto/ecdh"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"

	"golang.org/x/crypto/hkdf"
)

const seedSize = 32

func GenerateMLKEMKeypair() (*mlkem.DecapsulationKey1024, *mlkem.EncapsulationKey1024, error) {
	priv, err := mlkem.GenerateKey1024()
	if err != nil {
		return nil, nil, err
	}
	pub := priv.EncapsulationKey()
	return priv, pub, nil
}

func GenerateX25519Keypair() (*ecdh.PrivateKey, *ecdh.PublicKey, error) {
	curve := ecdh.X25519()
	priv, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	pub := priv.PublicKey()
	return priv, pub, nil
}

func X25519Encapsulate(seed []byte, senderPriv *ecdh.PrivateKey, recipientPub *ecdh.PublicKey) ([]byte, error) {
	if len(seed) != seedSize {
		return nil, errors.New("seed must be 32 bytes")
	}

	shared, err := senderPriv.ECDH(recipientPub)
	if err != nil {
		return nil, err
	}

	mask := make([]byte, seedSize)
	hkdfReader := hkdf.New(sha512.New, shared, nil, []byte("x25519-shared-seed-wrap"))
	if _, err := io.ReadFull(hkdfReader, mask); err != nil {
		return nil, err
	}

	ciphertext := make([]byte, seedSize)
	for i := 0; i < seedSize; i++ {
		ciphertext[i] = seed[i] ^ mask[i]
	}
	return ciphertext, nil
}

func X25519Decapsulate(ciphertext []byte, recipientPriv *ecdh.PrivateKey, senderPub *ecdh.PublicKey) ([]byte, error) {
	if len(ciphertext) != seedSize {
		return nil, errors.New("ciphertext must be 32 bytes")
	}

	shared, err := recipientPriv.ECDH(senderPub)
	if err != nil {
		return nil, err
	}

	mask := make([]byte, seedSize)
	hkdfReader := hkdf.New(sha512.New, shared, nil, []byte("x25519-shared-seed-wrap"))
	if _, err := io.ReadFull(hkdfReader, mask); err != nil {
		return nil, err
	}

	seed := make([]byte, seedSize)
	for i := range seedSize {
		seed[i] = ciphertext[i] ^ mask[i]
	}

	return seed, nil
}

func ValidateSeed(s []byte) error {
	if len(s) == 0 {
		return errors.New("shared seed must not be empty")
	}

	if len(s) < seedSize {
		return fmt.Errorf("shared seed too small: %v", len(s))
	}

	return nil
}

func deriveKeyInternal(hash func() hash.Hash, label string, size int, secrets ...[]byte) ([]byte, error) {
	total := 0
	for _, s := range secrets {
		if err := ValidateSeed(s); err != nil {
			return nil, err
		}

		total += len(s)
	}

	seed := make([]byte, total)
	offset := 0
	for _, s := range secrets {
		copy(seed[offset:], s)
		offset += len(s)
	}

	hkdfStream := hkdf.New(hash, seed, nil, []byte(label))
	key := make([]byte, size)
	if _, err := io.ReadFull(hkdfStream, key); err != nil {
		return nil, err
	}
	return key, nil
}

// DeriveChaCha20Key derives a 32-byte symmetric key using SHA-512 over the given secrets.
func DeriveChaCha20Key(sharedSecrets ...[]byte) ([]byte, error) {
	return deriveKeyInternal(sha512.New, "KeibiDrop-ChaCha20-Poly1305-SEK", KeySize, sharedSecrets...)
}

func Fingerprint(pub []byte) string {
	sum := sha512.Sum512(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ProtocolFingerprintV0 computes a stable fingerprint hash of ordered public keys.
func ProtocolFingerprintV0(pubkeys map[string][]byte) (string, error) {
	// Deterministic key order
	orderedKeys := []string{"x25519", "mlkem"}

	totalLen := 0
	for _, key := range orderedKeys {
		val, ok := pubkeys[key]
		if !ok || len(val) == 0 {
			return "", errors.New("missing or empty public key: " + key)
		}
		totalLen += len(val)
	}

	concat := make([]byte, totalLen)
	offset := 0
	for _, key := range orderedKeys {
		val := pubkeys[key]
		copy(concat[offset:], val)
		offset += len(val)
	}

	sum := sha512.Sum512(concat)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

type OwnKeys struct {
	MlKemPrivate *mlkem.DecapsulationKey1024
	MlKemPublic  *mlkem.EncapsulationKey1024

	X25519Private *ecdh.PrivateKey
	X25519Public  *ecdh.PublicKey
}

func (ok *OwnKeys) Validate() error {
	if ok.MlKemPrivate == nil || ok.MlKemPublic == nil || ok.X25519Private == nil || ok.X25519Public == nil {
		return fmt.Errorf("nil keys")
	}

	return nil
}

func (ok *OwnKeys) Fingerprint() (string, error) {
	if ok.MlKemPublic == nil || ok.X25519Public == nil {
		return "", fmt.Errorf("no registered pks")
	}

	pks := map[string][]byte{
		"x25519": ok.X25519Public.Bytes(),
		"mlkem":  ok.MlKemPublic.Bytes(),
	}

	return ProtocolFingerprintV0(pks)
}

func (ok *OwnKeys) ExportPubKeysAsMap() (map[string]string, error) {
	err := ok.Validate()
	if err != nil {
		return nil, err
	}

	res := map[string]string{
		"x25519": base64.RawURLEncoding.EncodeToString(ok.X25519Public.Bytes()),
		"mlkem":  base64.RawURLEncoding.EncodeToString(ok.MlKemPublic.Bytes()),
	}

	return res, nil
}

type PeerKeys struct {
	MlKemPublic  *mlkem.EncapsulationKey1024
	X25519Public *ecdh.PublicKey
}

func (pk *PeerKeys) Fingerprint() (string, error) {
	if pk.MlKemPublic == nil || pk.X25519Public == nil {
		return "", fmt.Errorf("no registered pks")
	}

	pks := map[string][]byte{
		"x25519": pk.X25519Public.Bytes(),
		"mlkem":  pk.MlKemPublic.Bytes(),
	}

	return ProtocolFingerprintV0(pks)
}

func (pk *PeerKeys) Validate() error {
	if pk.MlKemPublic == nil || pk.X25519Public == nil {
		return fmt.Errorf("nil keys")
	}

	return nil
}

func ParsePeerKeys(pubMap map[string][]byte) (*PeerKeys, error) {
	mlkemBytes, ok := pubMap["mlkem"]
	if !ok {
		return nil, errors.New("missing mlkem public key")
	}
	x25519Bytes, ok := pubMap["x25519"]
	if !ok {
		return nil, errors.New("missing x25519 public key")
	}

	mlkemPub, err := mlkem.NewEncapsulationKey1024(mlkemBytes)
	if err != nil {
		return nil, err
	}

	x25519Curve := ecdh.X25519()
	x25519Pub, err := x25519Curve.NewPublicKey(x25519Bytes)
	if err != nil {
		return nil, err
	}

	return &PeerKeys{
		MlKemPublic:  mlkemPub,
		X25519Public: x25519Pub,
	}, nil
}
