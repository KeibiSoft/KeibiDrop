// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package transport

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQUICListenerReleasesPort guards a regression where closing a QUIC listener did not
// release its UDP socket, so rebinding the same port failed with "address already in use".
func TestQUICListenerReleasesPort(t *testing.T) {
	ln, err := QUIC().Listen("127.0.0.1:0")
	require.NoError(t, err, "first listen")
	addr := ln.Addr().String()
	require.NoError(t, ln.Close(), "close")

	// Rebind the SAME port; must succeed now that the socket is released.
	ln2, err := QUIC().Listen(addr)
	require.NoError(t, err, "re-listen on %s after close (port not released)", addr)
	require.NoError(t, ln2.Close(), "second close")
}
