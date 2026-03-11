// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package tests

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// MockRelay is a test-local relay server that implements the KeibiDrop
// relay protocol (register/fetch with bearer-token lookup).
type MockRelay struct {
	Server *httptest.Server
	mu     sync.RWMutex
	store  map[string]string // lookupToken -> blob JSON
}

// NewMockRelay creates and starts a mock relay server.
func NewMockRelay() *MockRelay {
	mr := &MockRelay{
		store: make(map[string]string),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", mr.handleRegister)
	mux.HandleFunc("/fetch", mr.handleFetch)
	mr.Server = httptest.NewServer(mux)

	return mr
}

// Close shuts down the mock relay.
func (mr *MockRelay) Close() {
	mr.Server.Close()
}

// URL returns the relay base URL.
func (mr *MockRelay) URL() string {
	return mr.Server.URL
}

// EntryCount returns how many registrations are stored.
func (mr *MockRelay) EntryCount() int {
	mr.mu.RLock()
	defer mr.mu.RUnlock()
	return len(mr.store)
}

// Clear removes all stored registrations.
func (mr *MockRelay) Clear() {
	mr.mu.Lock()
	defer mr.mu.Unlock()
	mr.store = make(map[string]string)
}

func (mr *MockRelay) extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(auth, "Bearer ")
}

func (mr *MockRelay) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	token := mr.extractToken(r)
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var payload struct {
		Blob string `json:"blob"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Blob == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	mr.mu.Lock()
	mr.store[token] = string(body)
	mr.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
}

func (mr *MockRelay) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	token := mr.extractToken(r)
	if token == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	mr.mu.RLock()
	data, ok := mr.store[token]
	mr.mu.RUnlock()

	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(data))
}
