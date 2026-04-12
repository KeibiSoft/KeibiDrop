// ABOUTME: Android-specific network helpers that avoid net.Interfaces() which triggers
// ABOUTME: SELinux-denied netlink_route_socket bind operations on Android (b/155595000)

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build android

package common

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// inet6Entry holds a parsed line from /proc/net/if_inet6.
type inet6Entry struct {
	ip    net.IP
	iface string
	scope int
}

// parseIfInet6 reads /proc/net/if_inet6 to discover IPv6 addresses without
// using net.Interfaces() (which requires netlink, blocked by SELinux on Android).
func parseIfInet6() ([]inet6Entry, error) {
	f, err := os.Open("/proc/net/if_inet6")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var results []inet6Entry

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Format: address device_number prefix_len scope flags interface_name
		// Example: fec000000000000050540000fe123456 0f 40 40 20 eth0
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		addrHex := fields[0]
		scopeHex := fields[3]
		ifName := fields[5]

		if len(addrHex) != 32 {
			continue
		}

		addrBytes, err := hex.DecodeString(addrHex)
		if err != nil {
			continue
		}

		ip := net.IP(addrBytes)
		scope, _ := strconv.ParseInt(scopeHex, 16, 32)
		results = append(results, inet6Entry{ip: ip, iface: ifName, scope: int(scope)})
	}
	return results, nil
}

// GetLinkLocalAddress returns a link-local IPv6 address for direct LAN
// connections. On Android, we parse /proc/net/if_inet6 instead of using
// net.Interfaces().
func GetLinkLocalAddress(port int) (string, error) {
	entries, err := parseIfInet6()
	if err != nil {
		return net.JoinHostPort("::1", strconv.Itoa(port)), nil
	}

	for _, e := range entries {
		if e.ip.IsLinkLocalUnicast() && e.iface != "lo" && e.iface != "dummy0" {
			return fmt.Sprintf("%s%%%s:%d", e.ip.String(), e.iface, port), nil
		}
	}
	return net.JoinHostPort("::1", strconv.Itoa(port)), nil
}

// GetGlobalIPv6 returns a global-scope IPv6 address. On Android, we first
// try a UDP dial probe, then fall back to parsing /proc/net/if_inet6.
func GetGlobalIPv6() (string, error) {
	// Try dial probe first (works when IPv6 connectivity exists).
	conn, err := net.Dial("udp6", "[2001:4860:4860::8888]:80")
	if err == nil {
		defer conn.Close()
		host, _, err := net.SplitHostPort(conn.LocalAddr().String())
		if err == nil && host != "::1" {
			return host, nil
		}
	}

	// Fall back to /proc/net/if_inet6 for any non-loopback, non-link-local address.
	entries, err := parseIfInet6()
	if err != nil {
		return "::1", nil
	}

	// Prefer global scope, then site-local, then link-local.
	for _, e := range entries {
		if e.ip.IsGlobalUnicast() && !e.ip.IsLinkLocalUnicast() && e.iface != "lo" {
			return e.ip.String(), nil
		}
	}
	// Accept site-local (fec0::/10) addresses (common in emulators).
	for _, e := range entries {
		if !e.ip.IsLoopback() && !e.ip.IsLinkLocalUnicast() && e.iface != "lo" {
			return e.ip.String(), nil
		}
	}
	// Accept any non-loopback.
	for _, e := range entries {
		if !e.ip.IsLoopback() && e.iface != "lo" && e.iface != "dummy0" {
			return e.ip.String(), nil
		}
	}
	return "::1", nil
}

// GetLocalIPv6 returns any available local IPv6 address. On Android, we
// use /proc/net/if_inet6 to avoid netlink.
func GetLocalIPv6() (string, error) {
	ip, err := GetGlobalIPv6()
	if err != nil {
		return "", fmt.Errorf("no IPv6 address found")
	}
	return ip, nil
}
