// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build !darwin || ios

package discovery

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func (s *Service) mdnsBrowsePlatform(ctx context.Context) {
	s.mdnsBrowseMulticast(ctx)
}

func (s *Service) mdnsBrowseMulticast(ctx context.Context) {
	groupAddr, err := net.ResolveUDPAddr("udp4", mdnsAddr)
	if err != nil {
		return
	}

	listenConn, err := net.ListenMulticastUDP("udp4", nil, groupAddr)
	if err != nil {
		s.logger.Warn("mDNS: browse: failed to join multicast", "error", err)
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

	candidates := make(map[string]*mdnsCandidate)

	queryPacket, _ := buildPTRQuery(mdnsService)
	_, _ = sendConn.Write(queryPacket)

	ticker := time.NewTicker(mdnsQueryInterval)
	defer ticker.Stop()

	s.logger.Info("mDNS browser started (multicast)")

	buf := make([]byte, mdnsMaxPacket)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = sendConn.Write(queryPacket)
		default:
		}

		_ = listenConn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, readErr := listenConn.ReadFromUDP(buf)
		if readErr != nil {
			continue
		}

		msg, parseErr := dnsMessageParse(buf[:n])
		if parseErr != nil || msg.Header.Flags&dnsFlagQR == 0 {
			continue
		}

		var allRecords []dnsRecord
		allRecords = append(allRecords, msg.Answers...)
		allRecords = append(allRecords, msg.Additional...)
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
