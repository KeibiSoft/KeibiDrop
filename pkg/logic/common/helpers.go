// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package common

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/KeibiSoft/KeibiDrop/pkg/config"
)

// preferredLANIfaces lists interface-name prefixes to prefer when choosing a
// LAN address: physical Wi-Fi/Ethernet first. Shared by GetLinkLocalAddress,
// GetGlobalIPv6, and GetDiscoveryLANAddress (intentionally the same order).
var preferredLANIfaces = []string{"en0", "eth0", "wlan0", "en1", "wlp", "enp"}

// GetLinkLocalAddress finds a link-local IPv6 address on this machine and
// returns it formatted as "ip%zone:port" for direct LAN peer connections.
// Falls back to loopback (::1) when no link-local interface is available.
func GetLinkLocalAddress(port int) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// Collect all link-local candidates from non-loopback, up interfaces.
	type candidate struct {
		ip    net.IP
		iface string
	}
	var candidates []candidate
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if ip.To16() != nil && ip.To4() == nil && ip.IsLinkLocalUnicast() {
				candidates = append(candidates, candidate{ip: ip, iface: iface.Name})
			}
		}
	}

	if len(candidates) > 0 {
		// Prefer common LAN interfaces: en0 (macOS WiFi), eth0/wlan0 (Linux).
		// Avoid utun*, awdl*, llw*, ap* (VPN, AirDrop, low-latency WLAN, access point).
		preferred := preferredLANIfaces
		for _, pref := range preferred {
			for _, c := range candidates {
				if strings.HasPrefix(c.iface, pref) {
					return fmt.Sprintf("%s%%%s:%d", c.ip.String(), c.iface, port), nil
				}
			}
		}
		// No preferred match, pick first non-virtual candidate.
		for _, c := range candidates {
			skip := strings.HasPrefix(c.iface, "utun") ||
				strings.HasPrefix(c.iface, "awdl") ||
				strings.HasPrefix(c.iface, "llw")
			if !skip {
				return fmt.Sprintf("%s%%%s:%d", c.ip.String(), c.iface, port), nil
			}
		}
		// All candidates are virtual, use first one anyway.
		c := candidates[0]
		return fmt.Sprintf("%s%%%s:%d", c.ip.String(), c.iface, port), nil
	}

	// Fallback: accept loopback for local testing.
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if ip.To16() != nil && ip.To4() == nil && ip.IsLoopback() {
				return fmt.Sprintf("%s:%d", ip.String(), port), nil
			}
		}
	}

	return "", fmt.Errorf("no link-local or loopback IPv6 address found")
}

// virtualIfacePrefixes names interface kinds whose address LAN peers never see
// (Docker/VPN/bridges/AirDrop), so the discovery-address scan skips them.
var virtualIfacePrefixes = []string{"docker", "veth", "br-", "br0", "tun", "tap", "utun", "virbr", "wg", "awdl", "llw"}

func isVirtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// GetDiscoveryLANAddress returns this machine's private IPv4 on a preferred LAN
// interface, in the same form peers see it via discovery. The local-connect
// tiebreak uses it so equal names compare IPv4-vs-IPv4 (symmetric). Returns ""
// when no private IPv4 is available.
func GetDiscoveryLANAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	pick := func(prefix string) string {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
				continue
			}
			if isVirtualIface(iface.Name) {
				continue
			}
			if prefix != "" && !strings.HasPrefix(iface.Name, prefix) {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil {
					continue
				}
				if v4 := ip.To4(); v4 != nil && ip.IsPrivate() {
					return v4.String()
				}
			}
		}
		return ""
	}
	for _, pref := range preferredLANIfaces {
		if ip := pick(pref); ip != "" {
			return ip
		}
	}
	return pick("")
}

// ParsePeerDirectAddress parses a direct LAN peer address in any form the
// codebase produces or stores: "ip", "ip:port", "ip%zone", "ip%zone:port",
// "[ip]:port", or "[ip%zone]:port". Returns the bare IP with the zone always
// split off into zone ("" when absent) and the port (0 when absent). The IP
// must be link-local, loopback, or private; IPv6 link-local requires a zone.
func ParsePeerDirectAddress(addr string) (ip string, zone string, port int, err error) {
	if addr == "" {
		return "", "", 0, fmt.Errorf("empty address")
	}

	host := addr
	portStr := ""
	pct := strings.Index(addr, "%")
	colon := strings.LastIndex(addr, ":")
	switch {
	case strings.HasPrefix(addr, "["):
		// Bracketed "[ip]:port" or "[ip%zone]:port".
		host, portStr, err = net.SplitHostPort(addr)
		if err != nil {
			return "", "", 0, fmt.Errorf("invalid address %q: %w", addr, err)
		}
	case net.ParseIP(addr) != nil:
		// Bare IP, no zone, no port.
	case colon > pct:
		// "ipv4:port", "ip%zone:port", or unbracketed "ipv6:port" such as
		// "::1:26431": zones hold no colons, so the last colon splits the port.
		if colon == 0 {
			return "", "", 0, fmt.Errorf("invalid address format %q", addr)
		}
		host, portStr = addr[:colon], addr[colon+1:]
		if portStr == "" {
			return "", "", 0, fmt.Errorf("missing port in address %q", addr)
		}
	case pct != -1:
		// "ip%zone" without port, as stored by SetPeerDirectAddress.
	default:
		return "", "", 0, fmt.Errorf("invalid address format %q", addr)
	}

	// Split the zone off the host, if any.
	var zoned bool
	ip, zone, zoned = strings.Cut(host, "%")
	if zoned && zone == "" {
		return "", "", 0, fmt.Errorf("empty zone in address %q", addr)
	}

	parsedIP := net.ParseIP(ip)
	if parsedIP == nil {
		return "", "", 0, fmt.Errorf("invalid IP %q", ip)
	}
	// IPv4 with zone makes no sense, but accept for consistency.
	if !parsedIP.IsLinkLocalUnicast() && !parsedIP.IsLoopback() && !parsedIP.IsPrivate() {
		return "", "", 0, fmt.Errorf("address must be link-local, loopback, or private: %q", ip)
	}
	// A zoneless IPv6 link-local is ambiguous and undialable.
	if parsedIP.To4() == nil && parsedIP.IsLinkLocalUnicast() && zone == "" {
		return "", "", 0, fmt.Errorf("link-local address requires zone ID (%%iface): %q", addr)
	}

	if portStr != "" {
		port, err = strconv.Atoi(portStr)
		if err != nil {
			return "", "", 0, fmt.Errorf("invalid port %q: %w", portStr, err)
		}
		if !config.ValidPeerPort(port) {
			return "", "", 0, fmt.Errorf("port %d out of range %d-%d", port, config.MinPeerPort, config.MaxPeerPort)
		}
	}

	return ip, zone, port, nil
}

func GetLocalIPv6() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if ip.To16() != nil && ip.To4() == nil {
				// allow loopback + ULA + link-local
				return ip.String(), nil
			}
		}
	}
	return "", fmt.Errorf("no IPv6 address found")
}

// GetGlobalIPv6 returns a stable (non-temporary) global IPv6 address.
// On macOS/Linux, privacy extensions (RFC 4941) generate temporary addresses
// that rotate periodically. Connections bound to a temporary address break
// when the OS deprecates it. This function prefers stable addresses by
// collecting all candidates and picking the best one.
func GetGlobalIPv6() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	// Preferred interfaces for LAN (same order as GetLinkLocalAddress).
	preferred := preferredLANIfaces

	type candidate struct {
		ip    string
		iface string
	}
	var candidates []candidate

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip virtual/VPN interfaces.
		if strings.HasPrefix(iface.Name, "utun") ||
			strings.HasPrefix(iface.Name, "awdl") ||
			strings.HasPrefix(iface.Name, "llw") ||
			strings.HasPrefix(iface.Name, "docker") ||
			strings.HasPrefix(iface.Name, "br-") ||
			strings.HasPrefix(iface.Name, "veth") {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}
			if ip.To16() != nil && ip.To4() == nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				candidates = append(candidates, candidate{ip: ip.String(), iface: iface.Name})
			}
		}
	}

	if len(candidates) == 0 {
		// Fallback to any local IPv6 for testing.
		return GetLocalIPv6()
	}

	// Prefer candidates on preferred interfaces (en0, eth0, etc.).
	// The first address on a preferred interface is typically the stable one;
	// temporary addresses are appended after the stable/secured address.
	for _, pref := range preferred {
		for _, c := range candidates {
			if strings.HasPrefix(c.iface, pref) {
				return c.ip, nil
			}
		}
	}

	// No preferred interface match, return first candidate.
	return candidates[0].ip, nil
}

// GetLocalAddrs returns all private/link-local IP addresses for LAN discovery.
// These are included in the relay registration so peers on the same network
// can connect directly without going through the bridge relay.
func GetLocalAddrs() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var addrs []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip virtual/docker/VPN interfaces
		name := iface.Name
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "utun") {
			continue
		}

		ifAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range ifAddrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP
			// Private IPv4 (192.168.x.x, 10.x.x.x, 172.16-31.x.x)
			if ip.To4() != nil && ip.IsPrivate() {
				addrs = append(addrs, ip.String())
			}
			// Link-local IPv6 (fe80::)
			if ip.IsLinkLocalUnicast() && ip.To4() == nil {
				addrs = append(addrs, ip.String()+"%"+iface.Name)
			}
		}
	}
	return addrs
}

func ValidateFingerprint(fp string) error {
	if fp == "" {
		return ErrEmptyFingerprint
	}

	data, err := base64.RawURLEncoding.DecodeString(fp)
	if err != nil {
		return err
	}

	if len(data) != 64 {
		return ErrInvalidLength
	}

	return nil
}

func PostJSONWithURL(client *http.Client, endpoint *url.URL, headers map[string]string, payload interface{}, mapError ErrorMapperFunc) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, mapError(0, fmt.Errorf("failed to marshal JSON: %w", err))
	}

	req, err := http.NewRequest("POST", endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, mapError(0, fmt.Errorf("failed to create POST request: %w", err))
	}
	req.Header.Set("Content-Type", "application/json")
	for h, b := range headers {
		req.Header.Set(h, b)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, mapError(0, err)
	}

	if resp.StatusCode >= 400 {
		return resp, mapError(resp.StatusCode, nil)
	}

	return resp, nil
}

func GetJSONWithURL(client *http.Client, endpoint *url.URL, headers map[string]string, mapError ErrorMapperFunc) (*http.Response, error) {
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return nil, mapError(0, fmt.Errorf("failed to create GET request: %w", err))
	}

	for key, value := range headers {
		req.Header.Set(key, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, mapError(0, err)
	}

	if resp.StatusCode >= 400 {
		return resp, mapError(resp.StatusCode, nil)
	}

	return resp, nil
}
