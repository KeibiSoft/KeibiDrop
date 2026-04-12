// ABOUTME: Network interface helpers using net.Interfaces() (netlink-based).
// ABOUTME: Excluded on Android where SELinux blocks netlink_route_socket bind.

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !android

package common

import (
	"fmt"
	"net"
	"strings"
)

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
		preferred := []string{"en0", "eth0", "wlan0", "en1", "wlp", "enp"}
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

// GetLocalIPv6 returns any local IPv6 address (including loopback, link-local, ULA).
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

// GetGlobalIPv6 returns a global-scope IPv6 address, falling back to any local IPv6.
func GetGlobalIPv6() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}

	for _, iface := range interfaces {
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
				return ip.String(), nil
			}
		}
	}
	// Fallback to any local IPv6 (loopback, link-local, ULA) for local testing.
	return GetLocalIPv6()
}
