// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import "testing"

// TestQUICListenerReleasesPort guards a regression where closing a QUIC listener did not
// release its UDP socket, so rebinding the same port failed with "address already in use".
func TestQUICListenerReleasesPort(t *testing.T) {
	ln, err := QUIC().Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("first listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Rebind the SAME port; must succeed now that the socket is released.
	ln2, err := QUIC().Listen(addr)
	if err != nil {
		t.Fatalf("re-listen on %s after close (port not released): %v", addr, err)
	}
	if err := ln2.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}
