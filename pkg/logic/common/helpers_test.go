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

	"github.com/stretchr/testify/require"
)

func TestGetLinkLocalAddress(t *testing.T) {
	addr, err := GetLinkLocalAddress(26431)
	require.NoError(t, err)

	// Result must contain a zone separator (%) and a port separator (:).
	require.True(t, strings.Contains(addr, ":"), "expected address to contain ':', got %q", addr)
}

func TestGetLinkLocalAddress_PortInResult(t *testing.T) {
	addr, err := GetLinkLocalAddress(26999)
	require.NoError(t, err)

	// The port must appear at the very end after the last colon.
	lastColon := strings.LastIndex(addr, ":")
	require.NotEqual(t, -1, lastColon, "no colon in address %q", addr)
	portStr := addr[lastColon+1:]
	require.Equal(t, "26999", portStr, "expected port 26999 at end of address (full: %q)", addr)
}

func TestParsePeerDirectAddress_Valid(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("fe80::1%eth0:26431")
	require.NoError(t, err)
	require.Equal(t, "fe80::1", ip)
	require.Equal(t, "eth0", zone)
	require.Equal(t, 26431, port)
}

func TestParsePeerDirectAddress_ValidLongAddr(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("fe80::abcd:ef01:2345:6789%wlan0:26500")
	require.NoError(t, err)
	require.Equal(t, "fe80::abcd:ef01:2345:6789", ip)
	require.Equal(t, "wlan0", zone)
	require.Equal(t, 26500, port)
}

func TestParsePeerDirectAddress_Loopback(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("::1:26431")
	require.NoError(t, err)
	require.Equal(t, "::1", ip)
	require.Empty(t, zone)
	require.Equal(t, 26431, port)
}

func TestParsePeerDirectAddress_IPv4_PrivateAccepted(t *testing.T) {
	ip, zone, port, err := ParsePeerDirectAddress("192.168.1.1:26431")
	require.NoError(t, err, "unexpected error for private IPv4")
	require.Equal(t, "192.168.1.1", ip)
	require.Empty(t, zone)
	require.Equal(t, 26431, port)
}

func TestParsePeerDirectAddress_IPv4_PublicRejected(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("8.8.8.8:26431")
	require.Error(t, err, "expected error for public IPv4 address")
}

func TestParsePeerDirectAddress_BadPort_TooHigh(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:99999")
	require.Error(t, err, "expected error for port too high")
}

func TestParsePeerDirectAddress_BadPort_TooLow(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:100")
	require.Error(t, err, "expected error for port too low")
}

func TestParsePeerDirectAddress_BadPort_NotNumber(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0:abc")
	require.Error(t, err, "expected error for non-numeric port")
}

func TestParsePeerDirectAddress_MissingZone_LinkLocal(t *testing.T) {
	// fe80::1:26431 is ambiguous, it looks like part of an IPv6 address.
	// Should return an error because link-local without zone is unusable.
	_, _, _, err := ParsePeerDirectAddress("fe80::1:26431")
	require.Error(t, err, "expected error for link-local without zone")
}

func TestParsePeerDirectAddress_EmptyString(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("")
	require.Error(t, err, "expected error for empty string")
}

func TestParsePeerDirectAddress_JustPort(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress(":26431")
	require.Error(t, err, "expected error for just port")
}

func TestParsePeerDirectAddress_NoPort(t *testing.T) {
	// Port-less zoned form, exactly what SetPeerDirectAddress stores in PeerIPv6IP.
	ip, zone, port, err := ParsePeerDirectAddress("fe80::1%eth0")
	require.NoError(t, err)
	require.Equal(t, "fe80::1", ip)
	require.Equal(t, "eth0", zone)
	require.Zero(t, port)
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
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.ip, ip)
			require.Equal(t, c.zone, zone)
			require.Equal(t, c.port, port)
		})
	}
}
