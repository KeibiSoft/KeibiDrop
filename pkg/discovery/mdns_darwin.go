// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

//go:build darwin && !ios

package discovery

/*
#cgo LDFLAGS: -framework CoreFoundation
#include <dns_sd.h>
#include <string.h>
#include <stdlib.h>
#include <stdio.h>
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>

#define MAX_PEERS 32

typedef struct {
    char name[128];
    char host[256];
    uint16_t port;
    char ip[64];
    int ready;
} dnssd_peer;

static dnssd_peer g_peers[MAX_PEERS];
static int g_count = 0;

static void addrCB(DNSServiceRef ref, DNSServiceFlags flags, uint32_t ifIndex,
    DNSServiceErrorType err, const char *hostname, const struct sockaddr *addr,
    uint32_t ttl, void *ctx) {
    if (err != kDNSServiceErr_NoError) return;
    int idx = (int)(long)ctx;
    if (idx < 0 || idx >= MAX_PEERS) return;
    if (addr->sa_family == AF_INET) {
        const struct sockaddr_in *sin = (const struct sockaddr_in *)addr;
        unsigned char *b = (unsigned char *)&sin->sin_addr;
        snprintf(g_peers[idx].ip, sizeof(g_peers[idx].ip), "%d.%d.%d.%d", b[0], b[1], b[2], b[3]);
        g_peers[idx].ready = 1;
    }
}

static void resolveCB(DNSServiceRef ref, DNSServiceFlags flags, uint32_t ifIndex,
    DNSServiceErrorType err, const char *fullname, const char *hosttarget,
    uint16_t port, uint16_t txtLen, const unsigned char *txt, void *ctx) {
    if (err != kDNSServiceErr_NoError) return;
    int idx = (int)(long)ctx;
    if (idx < 0 || idx >= MAX_PEERS) return;
    g_peers[idx].port = ntohs(port);
    strncpy(g_peers[idx].host, hosttarget, sizeof(g_peers[idx].host)-1);
    // Now resolve hostname to IP
    DNSServiceRef addrRef = NULL;
    if (DNSServiceGetAddrInfo(&addrRef, kDNSServiceFlagsForceMulticast, ifIndex,
        kDNSServiceProtocol_IPv4, hosttarget, addrCB, (void*)(long)idx) == kDNSServiceErr_NoError && addrRef) {
        int afd = DNSServiceRefSockFD(addrRef);
        fd_set fds; struct timeval tv = {1, 0};
        FD_ZERO(&fds); FD_SET(afd, &fds);
        if (select(afd+1, &fds, NULL, NULL, &tv) > 0) DNSServiceProcessResult(addrRef);
        DNSServiceRefDeallocate(addrRef);
    }
}

static void browseCB(DNSServiceRef ref, DNSServiceFlags flags, uint32_t ifIndex,
    DNSServiceErrorType err, const char *name, const char *regtype, const char *domain, void *ctx) {
    if (err != kDNSServiceErr_NoError || !(flags & kDNSServiceFlagsAdd)) return;
    if (g_count >= MAX_PEERS) return;
    int idx = g_count++;
    memset(&g_peers[idx], 0, sizeof(dnssd_peer));
    strncpy(g_peers[idx].name, name, sizeof(g_peers[idx].name)-1);
    // Resolve immediately
    DNSServiceRef rRef = NULL;
    if (DNSServiceResolve(&rRef, kDNSServiceFlagsForceMulticast, ifIndex, name, regtype, domain,
        resolveCB, (void*)(long)idx) == kDNSServiceErr_NoError && rRef) {
        int rfd = DNSServiceRefSockFD(rRef);
        fd_set fds; struct timeval tv = {1, 0};
        FD_ZERO(&fds); FD_SET(rfd, &fds);
        if (select(rfd+1, &fds, NULL, NULL, &tv) > 0) DNSServiceProcessResult(rRef);
        DNSServiceRefDeallocate(rRef);
    }
}

static int dnssd_browse_once() {
    g_count = 0;
    memset(g_peers, 0, sizeof(g_peers));
    DNSServiceRef ref = NULL;
    if (DNSServiceBrowse(&ref, 0, 0, "_keibidrop._tcp", "local", browseCB, NULL) != kDNSServiceErr_NoError || !ref)
        return 0;
    int fd = DNSServiceRefSockFD(ref);
    fd_set fds; struct timeval tv;
    int i;
    for (i = 0; i < 10; i++) {
        tv.tv_sec = 0; tv.tv_usec = 300000;
        FD_ZERO(&fds); FD_SET(fd, &fds);
        if (select(fd+1, &fds, NULL, NULL, &tv) > 0)
            DNSServiceProcessResult(ref);
    }
    DNSServiceRefDeallocate(ref);
    return g_count;
}

static int dnssd_get_count() { return g_count; }
static const char* dnssd_get_name(int i) { return (i>=0 && i<g_count) ? g_peers[i].name : ""; }
static const char* dnssd_get_ip(int i) { return (i>=0 && i<g_count) ? g_peers[i].ip : ""; }
static int dnssd_get_port(int i) { return (i>=0 && i<g_count) ? g_peers[i].port : 0; }
static int dnssd_get_ready(int i) { return (i>=0 && i<g_count) ? g_peers[i].ready : 0; }
*/
import "C"
import (
	"context"
	"fmt"
	"net"
	"time"
)

func (s *Service) mdnsBrowsePlatform(ctx context.Context) {
	s.logger.Info("mDNS: using macOS DNS-SD API")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	s.dnssdBrowseOnce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.dnssdBrowseOnce()
		}
	}
}

func (s *Service) dnssdBrowseOnce() {
	count := int(C.dnssd_browse_once())
	for i := 0; i < count; i++ {
		name := C.GoString(C.dnssd_get_name(C.int(i)))
		ip := C.GoString(C.dnssd_get_ip(C.int(i)))
		port := int(C.dnssd_get_port(C.int(i)))
		ready := int(C.dnssd_get_ready(C.int(i)))

		if name == "" || name == s.name {
			continue
		}

		if ip == "" || port == 0 || ready == 0 {
			continue
		}
		parsed := net.ParseIP(ip)
		if parsed == nil || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() || parsed.IsLinkLocalMulticast() {
			continue
		}

		peerAddr := fmt.Sprintf("%s:%d", ip, port)
		s.mu.Lock()
		_, exists := s.peers[peerAddr]
		s.upsertPeer(name, peerAddr)
		s.mu.Unlock()
		if !exists {
			s.logger.Debug("mDNS: peer discovered", "name", name, "addr", peerAddr)
		}
	}
}
