// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// downloadRegistry tracks which partial downloads (.kdbitmap) belong to which peer.
// The peer identity is stored as an HMAC tag (not the raw fingerprint) so that
// even if the registry file is decrypted, the actual peer fingerprints are not exposed.
type downloadRegistry struct {
	mu      sync.Mutex
	entries map[string][16]byte
	path    string
	key     []byte
}

func newDownloadRegistry(configDir string, masterKey []byte) *downloadRegistry {
	r := &downloadRegistry{
		entries: make(map[string][16]byte),
	}
	if len(masterKey) > 0 && configDir != "" {
		h := sha256.Sum256(append(masterKey, []byte("download-registry")...))
		r.key = h[:]
		r.path = filepath.Clean(filepath.Join(configDir, ".kd_registry"))
		r.load()
	}
	return r
}

func (r *downloadRegistry) peerTag(peerFingerprint string, masterKey []byte) [16]byte {
	mac := hmac.New(sha256.New, masterKey)
	mac.Write([]byte(peerFingerprint))
	sum := mac.Sum(nil)
	var tag [16]byte
	copy(tag[:], sum[:16])
	return tag
}

func (r *downloadRegistry) Register(bitmapPath string, tag [16]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[filepath.Clean(bitmapPath)] = tag
	r.save()
}

func (r *downloadRegistry) Unregister(bitmapPath string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, filepath.Clean(bitmapPath))
	r.save()
}

func (r *downloadRegistry) ForPeer(tag [16]byte) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var paths []string
	for path, t := range r.entries {
		if t == tag {
			paths = append(paths, path)
		}
	}
	return paths
}

func (r *downloadRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *downloadRegistry) save() {
	if r.path == "" || r.key == nil {
		return
	}
	if len(r.entries) == 0 {
		_ = os.Remove(r.path)
		return
	}
	plaintext, _ := json.Marshal(r.entries)
	ct, err := encryptAESGCM(r.key, plaintext)
	if err != nil {
		return
	}
	_ = os.WriteFile(r.path, ct, 0600) // #nosec G306
}

func (r *downloadRegistry) load() {
	if r.path == "" || r.key == nil {
		return
	}
	data, err := os.ReadFile(r.path) // #nosec G304
	if err != nil {
		return
	}
	plaintext, err := decryptAESGCM(r.key, data)
	if err != nil {
		_ = os.Remove(r.path)
		return
	}
	if err := json.Unmarshal(plaintext, &r.entries); err != nil {
		r.entries = make(map[string][16]byte)
		return
	}
	if r.entries == nil {
		r.entries = make(map[string][16]byte)
	}
}

// sharedFilesStore persists what files were shared with which peer.
// Encrypted at rest, stored in configDir. Never in the shared/FUSE folder.
type sharedFilesStore struct {
	path string
	key  []byte
}

type sharedEntry struct {
	PeerTag  [16]byte `json:"t"`
	Path     string   `json:"p"`
	Size     uint64   `json:"s"`
	ModTime  uint64   `json:"m"`
}

func newSharedFilesStore(configDir string, masterKey []byte) *sharedFilesStore {
	if configDir == "" || len(masterKey) == 0 {
		return nil
	}
	h := sha256.Sum256(append(masterKey, []byte("shared-files-store")...))
	return &sharedFilesStore{
		path: filepath.Clean(filepath.Join(configDir, ".kd_shared")),
		key:  h[:],
	}
}

func (s *sharedFilesStore) Save(entries []sharedEntry) {
	if s == nil {
		return
	}
	if len(entries) == 0 {
		_ = os.Remove(s.path)
		return
	}
	data, _ := json.Marshal(entries)
	ct, err := encryptAESGCM(s.key, data)
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, ct, 0600) // #nosec G306
}

func (s *sharedFilesStore) Load() []sharedEntry {
	if s == nil {
		return nil
	}
	data, err := os.ReadFile(s.path) // #nosec G304
	if err != nil {
		return nil
	}
	pt, err := decryptAESGCM(s.key, data)
	if err != nil {
		_ = os.Remove(s.path)
		return nil
	}
	var entries []sharedEntry
	if err := json.Unmarshal(pt, &entries); err != nil {
		return nil
	}
	return entries
}

func (s *sharedFilesStore) Clear() {
	if s == nil {
		return
	}
	_ = os.Remove(s.path)
}

func encryptAESGCM(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func decryptAESGCM(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce := ciphertext[:gcm.NonceSize()]
	ct := ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}
