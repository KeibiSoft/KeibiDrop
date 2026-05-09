// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package discovery

import (
	"context"
	"fmt"
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
	groupAddr, err := net.ResolveUDPAddr("udp4", mdnsAddr)
	if err != nil {
		s.logger.Warn("mDNS: failed to resolve multicast addr", "error", err)
		return
	}

	listenConn, err := net.ListenMulticastUDP("udp4", nil, groupAddr)
	if err != nil {
		s.logger.Warn("mDNS: failed to join multicast group", "error", err)
		return
	}

	s.mu.Lock()
	s.mdnsConn = listenConn
	s.mu.Unlock()

	_ = listenConn.SetReadBuffer(mdnsMaxPacket * 4)

	sendConn, err := net.DialUDP("udp4", nil, groupAddr)
	if err != nil {
		s.logger.Warn("mDNS: failed to open send socket", "error", err)
		listenConn.Close()
		return
	}
	defer sendConn.Close()
	defer listenConn.Close()

	hostname := mdnsHostname()
	ourIPs := mdnsOurIPs()
	candidates := make(map[string]*mdnsCandidate)

	responsePacket, _ := buildServiceResponse(s.name, hostname, uint16(s.port), firstIPv4(ourIPs), mdnsTTL)

	queryPacket, _ := buildPTRQuery(mdnsService)
	_, _ = sendConn.Write(queryPacket)

	ticker := time.NewTicker(mdnsQueryInterval)
	defer ticker.Stop()

	s.logger.Info("mDNS started", "hostname", hostname, "ips", fmt.Sprint(ourIPs))

	buf := make([]byte, mdnsMaxPacket)
	for {
		select {
		case <-ctx.Done():
			goodbyePacket, _ := buildServiceResponse(s.name, hostname, uint16(s.port), firstIPv4(ourIPs), 0)
			if goodbyePacket != nil {
				_, _ = sendConn.Write(goodbyePacket)
			}
			return
		case <-ticker.C:
			_, _ = sendConn.Write(queryPacket)
		default:
		}

		_ = listenConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, src, readErr := listenConn.ReadFromUDP(buf)
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				continue
			}
			continue
		}

		msg, parseErr := dnsMessageParse(buf[:n])
		if parseErr != nil {
			continue
		}

		isQuery := msg.Header.Flags&dnsFlagQR == 0

		if isQuery {
			if !isFromSelf(src, ourIPs) && hasKeibidropQuestion(msg) && responsePacket != nil {
				_, _ = sendConn.Write(responsePacket)
			}
			continue
		}

		allRecords := append(msg.Answers, msg.Additional...)
		for _, r := range allRecords {
			switch r.Type {
			case dnsTypePTR:
				if !strings.HasSuffix(r.PtrName, mdnsService) {
					continue
				}
				instanceName := strings.TrimSuffix(r.PtrName, "."+mdnsService)
				if instanceName == s.name {
					continue
				}
				if r.TTL == 0 {
					s.removePeerByName(instanceName)
					delete(candidates, instanceName)
					continue
				}
				if _, ok := candidates[instanceName]; !ok {
					candidates[instanceName] = &mdnsCandidate{name: instanceName}
				}

			case dnsTypeSRV:
				instanceName := extractInstanceName(r.Name)
				if instanceName == "" || instanceName == s.name {
					continue
				}
				c, ok := candidates[instanceName]
				if !ok {
					c = &mdnsCandidate{name: instanceName}
					candidates[instanceName] = c
				}
				c.port = r.SrvPort

			case dnsTypeA:
				for _, c := range candidates {
					if c.ipv4 == nil {
						c.ipv4 = make(net.IP, len(r.AAddr))
						copy(c.ipv4, r.AAddr)
					}
				}
			}
		}

		for name, c := range candidates {
			if c.port > 0 && c.ipv4 != nil {
				peerAddr := fmt.Sprintf("%s:%d", c.ipv4.String(), c.port)
				s.mu.Lock()
				s.peers[peerAddr] = &Peer{
					Name:     c.name,
					Addr:     peerAddr,
					LastSeen: time.Now(),
				}
				s.mu.Unlock()
				delete(candidates, name)
			}
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

func (s *Service) removePeerByName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for addr, p := range s.peers {
		if p.Name == name {
			delete(s.peers, addr)
			return
		}
	}
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
