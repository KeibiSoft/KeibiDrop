package transport

import "testing"

// TestQUICListenerReleasesPort proves closing a QUIC listener frees its UDP port
// immediately, so a reconnect (or the next test) can rebind the same port. The bug it
// guards: quic.ListenAddr's listener Close did not release the internally-owned socket,
// so rebinding the same port failed with "address already in use".
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
