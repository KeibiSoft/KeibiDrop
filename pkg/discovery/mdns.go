// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package discovery

import (
	"context"
	"net"
	"os"
	"strings"
	"time"
	"unicode"
)

const (
	mdnsQueryInterval = 5 * time.Second
	mdnsTTL           = 120
	mdnsMaxPacket     = 4096
)

type mdnsCandidate struct {
	name string
	port uint16
	ipv4 net.IP
}

func (s *Service) mdnsRun(ctx context.Context) {
	go s.mdnsRespond(ctx)
	s.mdnsBrowsePlatform(ctx)
}

func (s *Service) mdnsRespond(ctx context.Context) {
	groupAddr, err := net.ResolveUDPAddr("udp4", mdnsAddr)
	if err != nil {
		return
	}

	listenConn, err := net.ListenMulticastUDP("udp4", nil, groupAddr)
	if err != nil {
		s.logger.Warn("mDNS: respond: failed to join multicast", "error", err)
		return
	}
	defer listenConn.Close()
	_ = listenConn.SetReadBuffer(mdnsMaxPacket * 4)

	sendConn, err := net.DialUDP("udp4", nil, groupAddr)
	if err != nil {
		listenConn.Close()
		return
	}
	defer sendConn.Close()

	hostname := mdnsHostname()
	ourIPs := mdnsOurIPs()
	responsePacket, _ := buildServiceResponse(s.name, hostname, uint16(s.port), firstIPv4(ourIPs), mdnsTTL)

	s.logger.Info("mDNS responder started", "hostname", hostname)

	buf := make([]byte, mdnsMaxPacket)
	for {
		select {
		case <-ctx.Done():
			goodbyePacket, _ := buildServiceResponse(s.name, hostname, uint16(s.port), firstIPv4(ourIPs), 0)
			if goodbyePacket != nil {
				_, _ = sendConn.Write(goodbyePacket)
			}
			return
		default:
		}

		_ = listenConn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, src, readErr := listenConn.ReadFromUDP(buf)
		if readErr != nil {
			continue
		}

		msg, parseErr := dnsMessageParse(buf[:n])
		if parseErr != nil {
			continue
		}

		if msg.Header.Flags&dnsFlagQR == 0 && !isFromSelf(src, ourIPs) && hasKeibidropQuestion(msg) && responsePacket != nil {
			_, _ = sendConn.Write(responsePacket)
		}
	}
}

func hasKeibidropQuestion(msg *dnsMessage) bool {
	for _, q := range msg.Questions {
		if q.Name == mdnsService && q.Type == dnsTypePTR {
			return true
		}
	}
	return false
}

func isFromSelf(src *net.UDPAddr, ourIPs []net.IP) bool {
	if src == nil {
		return false
	}
	for _, ip := range ourIPs {
		if ip.Equal(src.IP) {
			return true
		}
	}
	return false
}

func extractInstanceName(fullName string) string {
	suffix := "." + mdnsService
	if strings.HasSuffix(fullName, suffix) {
		return strings.TrimSuffix(fullName, suffix)
	}
	return ""
}

func mdnsHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "keibidrop-peer"
	}
	h = strings.Split(h, ".")[0]
	var clean []rune
	for _, r := range strings.ToLower(h) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			clean = append(clean, r)
		}
	}
	if len(clean) == 0 {
		return "keibidrop-peer"
	}
	return string(clean)
}

func mdnsOurIPs() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 != nil && isPrivateIP(ip4) {
				ips = append(ips, ip4)
			}
		}
	}
	return ips
}

func isPrivateIP(ip net.IP) bool {
	private := []net.IPNet{
		{IP: net.IP{10, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},
		{IP: net.IP{172, 16, 0, 0}, Mask: net.CIDRMask(12, 32)},
		{IP: net.IP{192, 168, 0, 0}, Mask: net.CIDRMask(16, 32)},
	}
	for _, p := range private {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func firstIPv4(ips []net.IP) net.IP {
	for _, ip := range ips {
		if ip4 := ip.To4(); ip4 != nil {
			return ip4
		}
	}
	return net.IP{127, 0, 0, 1}
}
