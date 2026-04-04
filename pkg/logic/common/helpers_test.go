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

func TestParsePeerDirectAddress_IPv4_Rejected(t *testing.T) {
	_, _, _, err := ParsePeerDirectAddress("192.168.1.1:26431")
	if err == nil {
		t.Fatal("expected error for IPv4 address, got nil")
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
	_, _, _, err := ParsePeerDirectAddress("fe80::1%eth0")
	if err == nil {
		t.Fatal("expected error for missing port, got nil")
	}
}
