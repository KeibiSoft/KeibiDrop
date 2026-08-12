// ABOUTME: Tests the reconnect transport ordering: PreferDirect gates direct-first,
// ABOUTME: and a failed direct attempt flips later attempts to bridge-first.
package session

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
)

func newPolicyTestManager() *ReconnectManager {
	return NewReconnectManager(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestUseBridgeFirst(t *testing.T) {
	dial := func(string) (net.Conn, error) { return nil, fmt.Errorf("test dial") }

	cases := []struct {
		name         string
		bridge       string
		dial         func(string) (net.Conn, error)
		preferDirect func() bool
		fellBack     bool
		want         bool
	}{
		{name: "no bridge configured", want: false},
		{name: "bridge without dial func", bridge: "b:26600", want: false},
		{name: "bridge, no policy (legacy)", bridge: "b:26600", dial: dial, want: true},
		{name: "bridge, prefers direct", bridge: "b:26600", dial: dial, preferDirect: func() bool { return true }, want: false},
		{name: "bridge, policy says bridge", bridge: "b:26600", dial: dial, preferDirect: func() bool { return false }, want: true},
		{name: "direct already failed this outage", bridge: "b:26600", dial: dial, preferDirect: func() bool { return true }, fellBack: true, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newPolicyTestManager()
			r.BridgeAddr = tc.bridge
			r.DialBridge = tc.dial
			r.PreferDirect = tc.preferDirect
			r.fallbackToBridge.Store(tc.fellBack)
			if got := r.useBridgeFirst(); got != tc.want {
				t.Fatalf("useBridgeFirst() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDirectFailureFlipsToBridge(t *testing.T) {
	r := newPolicyTestManager()
	r.session = &Session{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	r.BridgeAddr = "b:26600"
	dialed := 0
	r.DialBridge = func(string) (net.Conn, error) {
		dialed++
		return nil, fmt.Errorf("bridge down in test")
	}
	r.PreferDirect = func() bool { return true }

	// No cached peer, no relay lookup: the direct attempt fails without the
	// bridge being touched, and flags the next attempt for bridge-first.
	if err := r.reconnectAsInitiator(); err == nil {
		t.Fatal("expected the direct attempt to fail")
	}
	if dialed != 0 {
		t.Fatalf("bridge dialed during the direct attempt (%d times)", dialed)
	}
	if !r.fallbackToBridge.Load() {
		t.Fatal("direct failure did not flag bridge-first")
	}

	if err := r.reconnectAsInitiator(); err == nil {
		t.Fatal("expected the bridge attempt to fail with the stub dialer")
	}
	if dialed == 0 {
		t.Fatal("second attempt did not use the bridge")
	}
}
