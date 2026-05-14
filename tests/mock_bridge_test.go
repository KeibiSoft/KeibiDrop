// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Mock bridge relay for integration tests.
// ABOUTME: Pairs TCP connections by matching 32-byte room tokens.

package tests

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// MockBridge is a minimal TCP bridge that pairs connections by room token.
// Each client sends a 32-byte token; two clients with the same token are
// paired and their data is relayed bidirectionally.
type MockBridge struct {
	listener net.Listener
	mu       sync.Mutex
	pending  map[[32]byte]net.Conn
	done     chan struct{}
}

// NewMockBridge starts a mock bridge on a random port.
func NewMockBridge() (*MockBridge, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	mb := &MockBridge{
		listener: ln,
		pending:  make(map[[32]byte]net.Conn),
		done:     make(chan struct{}),
	}
	go mb.acceptLoop()
	return mb, nil
}

// Addr returns the bridge's listen address.
func (mb *MockBridge) Addr() string {
	return mb.listener.Addr().String()
}

// Close shuts down the bridge.
func (mb *MockBridge) Close() {
	mb.listener.Close()
	<-mb.done
}

func (mb *MockBridge) acceptLoop() {
	defer close(mb.done)
	for {
		conn, err := mb.listener.Accept()
		if err != nil {
			return
		}
		go mb.handleConn(conn)
	}
}

func (mb *MockBridge) handleConn(conn net.Conn) {
	var token [32]byte
	if _, err := io.ReadFull(conn, token[:]); err != nil {
		conn.Close()
		return
	}

	mb.mu.Lock()
	if peer, ok := mb.pending[token]; ok {
		delete(mb.pending, token)
		mb.mu.Unlock()
		go relay(conn, peer)
		return
	}
	mb.pending[token] = conn
	mb.mu.Unlock()
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		io.Copy(dst, src)
		dst.Close()
	}
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}

// PendingCount returns how many unpaired connections are waiting.
func (mb *MockBridge) PendingCount() int {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return len(mb.pending)
}

// FormatAddr returns "host:port" suitable for KeibiDrop.BridgeAddr.
func (mb *MockBridge) FormatAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", mb.listener.Addr().(*net.TCPAddr).Port)
}
