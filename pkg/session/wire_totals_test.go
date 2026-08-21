// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: WireTotals must survive conn replacement and teardown, the exact
// ABOUTME: event that silently zeroed the per-conn wire counters in the field.

package session

import (
	"io"
	"net"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	kbc "github.com/KeibiSoft/KeibiDrop/pkg/crypto"
	"github.com/stretchr/testify/require"
)

// movePipeBytes writes n bytes through a SecureConn pair and returns the
// plaintext count each end recorded.
func movePipeBytes(t *testing.T, from, to *SecureConn, n int) {
	t.Helper()
	msg := testkit.RandBytes(t, n)
	errCh := make(chan error, 1)
	go func() { _, e := from.Write(msg); errCh <- e }()
	got := make([]byte, n)
	_, err := io.ReadFull(to, got)
	require.NoError(t, err)
	require.NoError(t, <-errCh)
}

func newTotalsConnPair(t *testing.T) (*SecureConn, *SecureConn) {
	t.Helper()
	key := testkit.RandBytes(t, 32)
	c1, c2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close() })
	return NewSecureConn(c1, key, kbc.CipherAES256, NoncePrefixOutbound),
		NewSecureConn(c2, key, kbc.CipherAES256, NoncePrefixInbound)
}

// TestWireTotals_SurviveConnReplacement: bytes moved on a conn stay in the
// session total after the conn is replaced and after teardown to nil, across
// two generations. WireStats-style live reads lose them; WireTotals must not.
func TestWireTotals_SurviveConnReplacement(t *testing.T) {
	s := &Session{}

	gen1out, gen1in := newTotalsConnPair(t)
	s.SetOutboundConn(gen1out)
	movePipeBytes(t, gen1out, gen1in, 4096)

	sent, _ := s.WireTotals()
	require.EqualValues(t, 4096, sent, "live conn bytes must show in totals")

	// Reconnect: a fresh conn replaces the old one.
	gen2out, gen2in := newTotalsConnPair(t)
	s.SetOutboundConn(gen2out)
	sent, _ = s.WireTotals()
	require.EqualValues(t, 4096, sent, "replaced conn bytes must survive")

	movePipeBytes(t, gen2out, gen2in, 1000)
	sent, _ = s.WireTotals()
	require.EqualValues(t, 5096, sent, "totals accumulate across generations")

	// Teardown to nil: the counters must not vanish with the pointer.
	s.SetOutboundConn(nil)
	sent, _ = s.WireTotals()
	require.EqualValues(t, 5096, sent, "teardown must not zero the totals")

	// Idempotent re-set of the same conn must not double count.
	gen3out, _ := newTotalsConnPair(t)
	s.SetOutboundConn(gen3out)
	s.SetOutboundConn(gen3out)
	sent, _ = s.WireTotals()
	require.EqualValues(t, 5096, sent, "re-setting the same conn must not double count")
}

// TestWireTotals_BothDirections: inbound and outbound accumulate
// independently and sum recv as well as sent.
func TestWireTotals_BothDirections(t *testing.T) {
	s := &Session{}
	out, outPeer := newTotalsConnPair(t)
	in, inPeer := newTotalsConnPair(t)
	s.SetOutboundConn(out)
	s.SetInboundConn(in)

	movePipeBytes(t, out, outPeer, 2048) // We send on the outbound conn.
	movePipeBytes(t, inPeer, in, 512)    // The peer sends to our inbound conn.

	sent, recv := s.WireTotals()
	require.EqualValues(t, 2048, sent)
	require.EqualValues(t, 512, recv)

	s.SetOutboundConn(nil)
	s.SetInboundConn(nil)
	sent, recv = s.WireTotals()
	require.EqualValues(t, 2048, sent)
	require.EqualValues(t, 512, recv)
}
