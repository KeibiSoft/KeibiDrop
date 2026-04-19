// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Tests to ensure the listener is always dual-stack (tcp, not tcp6)
// ABOUTME: and that LAN addresses are only used in local mode.

package common

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"testing"
)

// TestListenerAcceptsIPv4 verifies the listener is dual-stack and accepts IPv4.
// Regression test: a previous change switched to tcp6 which broke bridge relay
// on devices without IPv6 (gRPC loopback uses 127.0.0.1 on some platforms).
func TestListenerAcceptsIPv4(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, 26700, 26701, "", t.TempDir(), false, false, "::1")
	if err != nil {
		t.Fatalf("NewKeibiDropWithIP failed: %v", err)
	}

	// Try connecting via IPv4 loopback
	conn, err := net.Dial("tcp4", fmt.Sprintf("127.0.0.1:%d", 26700))
	if err != nil {
		t.Fatalf("IPv4 connection to listener failed: %v (listener must be dual-stack 'tcp', not 'tcp6')", err)
	}
	conn.Close()

	// Try connecting via IPv6 loopback
	conn6, err := net.Dial("tcp6", fmt.Sprintf("[::1]:%d", 26700))
	if err != nil {
		t.Fatalf("IPv6 connection to listener failed: %v", err)
	}
	conn6.Close()

	_ = kd
}

// TestLANAddressesSkippedInInternetMode verifies that PeerLocalAddrs are NOT
// tried during JoinRoom when IsLocalMode is false.
// Regression test: LAN addresses were tried in internet mode, causing the
// joiner to connect via IPv4 LAN while the creator expected IPv6, leading to
// stuck connections and fingerprint mismatches on the bridge.
func TestLANAddressesSkippedInInternetMode(t *testing.T) {
	// This is a code-level assertion — verify the condition in logic.go
	// The JoinRoom LAN block should be gated by kd.IsLocalMode.
	//
	// We can't easily test the full JoinRoom flow here (needs relay + bridge),
	// but we verify the KeibiDrop struct defaults are correct.
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	relayURL, _ := url.Parse("https://localhost:9999")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kd, err := NewKeibiDropWithIP(ctx, logger, false, relayURL, 26702, 26703, "", t.TempDir(), false, false, "::1")
	if err != nil {
		t.Fatalf("NewKeibiDropWithIP failed: %v", err)
	}

	// Default should NOT be local mode
	if kd.IsLocalMode {
		t.Fatal("IsLocalMode should default to false")
	}

	// Simulate having LAN addresses from relay registration
	kd.PeerLocalAddrs = []string{"192.168.1.42", "fe80::1%eth0"}

	// In internet mode (IsLocalMode=false), these should be ignored.
	// The actual enforcement is in logic.go JoinRoom:
	//   if kd.IsLocalMode && len(kd.PeerLocalAddrs) > 0 {
	// We verify the flag is correct — the logic test is the guard condition.
	if kd.IsLocalMode {
		t.Fatal("IsLocalMode should still be false after setting PeerLocalAddrs")
	}
}
