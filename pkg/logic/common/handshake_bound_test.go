// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests that a failed inbound handshake closes the accepted connection
// ABOUTME: and a successful one leaves it open to become the session transport.

package common

import (
	"errors"
	"net"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recorderConn counts closes. Only Close is exercised, so the embedded nil Conn
// is never touched.
type recorderConn struct {
	net.Conn
	closes int
}

func (c *recorderConn) Close() error {
	c.closes++
	return nil
}

// stubHandshake swaps the handshake for the duration of one test.
func stubHandshake(t *testing.T, err error) {
	t.Helper()
	orig := performInboundHandshake
	performInboundHandshake = func(*session.Session, net.Conn) error { return err }
	t.Cleanup(func() { performInboundHandshake = orig })
}

// A failed handshake must not leak the accepted connection.
func TestHandshakeOrCloseClosesConnOnFailure(t *testing.T) {
	wantErr := errors.New("fingerprint mismatch")
	stubHandshake(t, wantErr)
	c := &recorderConn{}

	err := handshakeOrClose(nil, c)

	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, c.closes, "failed handshake must close the conn")
}

func TestHandshakeOrCloseKeepsConnOnSuccess(t *testing.T) {
	stubHandshake(t, nil)
	c := &recorderConn{}

	require.NoError(t, handshakeOrClose(nil, c))
	assert.Zero(t, c.closes, "a successful conn becomes the transport, keep it open")
}
