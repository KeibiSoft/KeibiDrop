// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
// ABOUTME: Tests for local-mode helper functions — address discovery and parsing.
// ABOUTME: Covers GetLinkLocalAddress and ParsePeerDirectAddress for LAN connectivity.
package common

import (
	"strings"
	"testing"
)

func TestGetLinkLocalAddress(t *testing.T) {
	addr, err := GetLinkLocalAddress(26431)
	if err != nil {
		t.Fatalf("GetLinkLocalAddress returned error: %v", err)
	}

	// Result must contain a zone separator (%) and a port separator (:).
	if !strings.Contains(addr, ":") {
		t.Errorf("expected address to contain ':', got %q", addr)
	}
}

func TestGetLinkLocalAddress_PortInResult(t *testing.T) {
	addr, err := GetLinkLocalAddress(26999)
	if err != nil {
		t.Fatalf("GetLinkLocalAddress returned error: %v", err)
	}

	// The port must appear at the very end after the last colon.
	lastColon := strings.LastIndex(addr, ":")
	if lastColon == -1 {
		t.Fatalf("no colon in address %q", addr)
	}
	portStr := addr[lastColon+1:]
	if portStr != "26999" {
		t.Errorf("expected port 26999 at end of address, got %q (full: %q)", portStr, addr)
	}
}

func TestParsePeerDirectAddress_Valid(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("fe80::1%eth0:26431")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "fe80::1" {
		t.Errorf("ip = %q, want %q", ip, "fe80::1")
	}
	if zone != "eth0" {
		t.Errorf("zone = %q, want %q", zone, "eth0")
	}
	if port != 26431 {
		t.Errorf("port = %d, want %d", port, 26431)
	}
}

func TestParsePeerDirectAddress_ValidLongAddr(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("fe80::abcd:ef01:2345:6789%wlan0:26500")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "fe80::abcd:ef01:2345:6789" {
		t.Errorf("ip = %q, want %q", ip, "fe80::abcd:ef01:2345:6789")
	}
	if zone != "wlan0" {
		t.Errorf("zone = %q, want %q", zone, "wlan0")
	}
	if port != 26500 {
		t.Errorf("port = %d, want %d", port, 26500)
	}
}

func TestParsePeerDirectAddress_Loopback(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("::1:26431")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "::1" {
		t.Errorf("ip = %q, want %q", ip, "::1")
	}
	if zone != "" {
		t.Errorf("zone = %q, want empty", zone)
	}
	if port != 26431 {
		t.Errorf("port = %d, want %d", port, 26431)
	}
}

func TestParsePeerDirectAddress_IPv4_PrivateAccepted(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("192.168.1.1:26431")
	if err != nil {
		t.Fatalf("unexpected error for private IPv4: %v", err)
	}
	if ip != "192.168.1.1" || zone != "" || port != 26431 {
		t.Fatalf("got ip=%q zone=%q port=%d", ip, zone, port)
	}
}

func TestParsePeerDirectAddress_IPv4_PublicRejected(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("8.8.8.8:26431")
	if err == nil {
		t.Fatal("expected error for public IPv4 address, got nil")
	}
}

func TestParsePeerDirectAddress_BadPort_TooHigh(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:99999")
	if err == nil {
		t.Fatal("expected error for port too high, got nil")
	}
}

func TestParsePeerDirectAddress_BadPort_TooLow(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:100")
	if err == nil {
		t.Fatal("expected error for port too low, got nil")
	}
}

func TestParsePeerDirectAddress_BadPort_NotNumber(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:abc")
	if err == nil {
		t.Fatal("expected error for non-numeric port, got nil")
	}
}

func TestParsePeerDirectAddress_MissingZone_LinkLocal(t *testing.T) {
	// fe80::1:26431 is ambiguous — looks like part of IPv6 address.
	// Should return an error because link-local without zone is unusable.
	_, _, _, err := ParsePeerDirectAddress("fe80::1:26431")
	if err == nil {
		t.Fatal("expected error for link-local without zone, got nil")
	}
}

func TestParsePeerDirectAddress_EmptyString(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("")
	if err == nil {
		t.Fatal("expected error for empty string, got nil")
	}
}

func TestParsePeerDirectAddress_JustPort(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress(":26431")
	if err == nil {
		t.Fatal("expected error for just port, got nil")
	}
}

func TestParsePeerDirectAddress_NoPort(t *testing.T) {
	// Port-less zoned form, exactly what SetPeerDirectAddress stores in PeerIPv6IP.
	ip, zone, port, err := ParsePeerDirectAddress("fe80::1%eth0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "fe80::1" || zone != "eth0" || port != 0 {
		t.Errorf("got ip=%q zone=%q port=%d, want ip=%q zone=%q port=0", ip, zone, port, "fe80::1", "eth0")
	}
}

// TestParsePeerDirectAddressForms covers every form the codebase produces:
// discovery ("ipv4:port"), GetLinkLocalAddress ("ip%zone:port", "::1:port"),
// the port-less forms stored in PeerIPv6IP, bracketed input, and garbage.
func TestParsePeerDirectAddressForms(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		ip      string
		zone    string
		port    int
		wantErr bool
	}{
		{"bare ipv4", "192.168.1.42", "192.168.1.42", "", 0, false},
		{"bare ipv4 ten-net", "10.0.0.5", "10.0.0.5", "", 0, false},
		{"ipv4 with port", "192.168.1.42:26431", "192.168.1.42", "", 26431, false},
		{"bare ipv6 loopback", "::1", "::1", "", 0, false},
		{"bare ipv6 ula", "fd00::5", "fd00::5", "", 0, false},
		{"ipv6 loopback with port", "::1:26431", "::1", "", 26431, false},
		{"zoned link-local", "fe80::1%eth0", "fe80::1", "eth0", 0, false},
		{"zoned link-local with port", "fe80::1%eth0:26431", "fe80::1", "eth0", 26431, false},
		{"bracketed ipv6 with port", "[fd00::5]:26431", "fd00::5", "", 26431, false},
		{"bracketed zoned with port", "[fe80::1%eth0]:26431", "fe80::1", "eth0", 26431, false},
		{"empty", "", "", "", 0, true},
		{"garbage", "not-an-address", "", "", 0, true},
		{"public ipv4", "203.0.113.9:26431", "", "", 0, true},
		{"public bare ipv6", "2001:db8::5", "", "", 0, true},
		{"zoneless link-local with port", "fe80::1:26431", "", "", 0, true},
		{"bare zoneless link-local", "fe80::1", "", "", 0, true},
		{"empty zone", "fe80::1%", "", "", 0, true},
		{"empty zone with port", "fe80::1%:26431", "", "", 0, true},
		{"port out of range", "192.168.1.42:80", "", "", 0, true},
		{"port not a number", "192.168.1.42:abc", "", "", 0, true},
		{"port zero", "192.168.1.42:0", "", "", 0, true},
		{"trailing colon", "192.168.1.42:", "", "", 0, true},
		{"just port", ":26431", "", "", 0, true},
		{"bracket without port", "[fd00::5]", "", "", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ip, zone, port, err := ParsePeerDirectAddress(c.addr)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ParsePeerDirectAddress(%q): expected error, got ip=%q zone=%q port=%d", c.addr, ip, zone, port)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePeerDirectAddress(%q): unexpected error: %v", c.addr, err)
			}
			if ip != c.ip || zone != c.zone || port != c.port {
				t.Errorf("ParsePeerDirectAddress(%q) = (%q, %q, %d), want (%q, %q, %d)", c.addr, ip, zone, port, c.ip, c.zone, c.port)
			}
		})
	}
}
