// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package discovery

import (
	"encoding/binary"
	"net"
	"testing"
)

func TestDNSNameEncode_Simple(t *testing.T) {
	got := dnsNameEncode("_keibidrop._tcp.local.")
	want := []byte{10, '_', 'k', 'e', 'i', 'b', 'i', 'd', 'r', 'o', 'p', 4, '_', 't', 'c', 'p', 5, 'l', 'o', 'c', 'a', 'l', 0}
	if len(got) != len(want) {
		t.Fatalf("length mismatch: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("byte %d: got %02x, want %02x", i, got[i], want[i])
		}
	}
}

func TestDNSNameEncode_SingleLabel(t *testing.T) {
	got := dnsNameEncode("local.")
	want := []byte{5, 'l', 'o', 'c', 'a', 'l', 0}
	if len(got) != len(want) {
		t.Fatalf("length: got %d want %d", len(got), len(want))
	}
}

func TestDNSNameEncode_Root(t *testing.T) {
	got := dnsNameEncode(".")
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("root: got %v, want [0]", got)
	}
}

func TestDNSNameEncode_WithSpaces(t *testing.T) {
	got := dnsNameEncode("Cosmic Waffle._keibidrop._tcp.local.")
	if got[0] != 13 {
		t.Errorf("first label length: got %d, want 13 (len of 'Cosmic Waffle')", got[0])
	}
	label := string(got[1:14])
	if label != "Cosmic Waffle" {
		t.Errorf("first label: got %q, want %q", label, "Cosmic Waffle")
	}
}

func TestDNSNameDecode_Roundtrip(t *testing.T) {
	names := []string{
		"_keibidrop._tcp.local.",
		"local.",
		"Cosmic Waffle._keibidrop._tcp.local.",
		"a.b.c.d.e.",
	}
	for _, name := range names {
		encoded := dnsNameEncode(name)
		decoded, off, err := dnsNameDecode(encoded, 0)
		if err != nil {
			t.Errorf("decode %q: %v", name, err)
			continue
		}
		if decoded != name {
			t.Errorf("roundtrip %q: got %q", name, decoded)
		}
		if off != len(encoded) {
			t.Errorf("offset %q: got %d, want %d", name, off, len(encoded))
		}
	}
}

func TestDNSNameDecode_Compressed(t *testing.T) {
	var buf []byte
	buf = append(buf, dnsNameEncode("_keibidrop._tcp.local.")...)
	nameStart := len(buf)
	buf = append(buf, 6, 'C', 'o', 's', 'm', 'i', 'c')
	buf = append(buf, 0xC0, 0x00)

	decoded, _, err := dnsNameDecode(buf, nameStart)
	if err != nil {
		t.Fatalf("compressed decode: %v", err)
	}
	want := "Cosmic._keibidrop._tcp.local."
	if decoded != want {
		t.Errorf("got %q, want %q", decoded, want)
	}
}

func TestDNSNameDecode_PointerLoop(t *testing.T) {
	buf := []byte{0xC0, 0x02, 0xC0, 0x00}
	_, _, err := dnsNameDecode(buf, 0)
	if err == nil {
		t.Error("expected error for pointer loop")
	}
}

func TestDNSNameDecode_Truncated(t *testing.T) {
	buf := []byte{5, 'h', 'e'}
	_, _, err := dnsNameDecode(buf, 0)
	if err == nil {
		t.Error("expected error for truncated label")
	}
}

func TestDNSNameDecode_Empty(t *testing.T) {
	_, _, err := dnsNameDecode([]byte{}, 0)
	if err == nil {
		t.Error("expected error for empty buffer")
	}
}

func TestDNSHeader_Roundtrip(t *testing.T) {
	h := dnsHeader{ID: 0, Flags: dnsFlagQR | dnsFlagAA, QDCount: 1, ANCount: 2, ARCount: 3}
	var buf [dnsHeaderLen]byte
	h.marshal(buf[:])

	var h2 dnsHeader
	if err := h2.unmarshal(buf[:]); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if h2 != h {
		t.Errorf("roundtrip: got %+v, want %+v", h2, h)
	}
}

func TestDNSMessage_QueryRoundtrip(t *testing.T) {
	msg := &dnsMessage{
		Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypePTR, Class: dnsClassIN}},
	}
	data, err := dnsMessageMarshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	parsed, err := dnsMessageParse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(parsed.Questions) != 1 {
		t.Fatalf("questions: got %d, want 1", len(parsed.Questions))
	}
	if parsed.Questions[0].Name != mdnsService {
		t.Errorf("name: got %q, want %q", parsed.Questions[0].Name, mdnsService)
	}
	if parsed.Questions[0].Type != dnsTypePTR {
		t.Errorf("type: got %d, want %d", parsed.Questions[0].Type, dnsTypePTR)
	}
}

func TestDNSMessage_ResponseRoundtrip(t *testing.T) {
	ip := net.IP{192, 168, 1, 42}
	data, err := buildServiceResponse("Swift Penguin", "myhost", 26531, ip, 120)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	parsed, err := dnsMessageParse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if parsed.Header.Flags&dnsFlagQR == 0 {
		t.Error("expected QR flag set")
	}
	if len(parsed.Answers) != 1 {
		t.Fatalf("answers: got %d, want 1", len(parsed.Answers))
	}
	ptr := parsed.Answers[0]
	if ptr.Type != dnsTypePTR {
		t.Errorf("answer type: got %d, want PTR (%d)", ptr.Type, dnsTypePTR)
	}
	if ptr.PtrName != "Swift Penguin."+mdnsService {
		t.Errorf("PTR name: got %q", ptr.PtrName)
	}

	if len(parsed.Additional) < 2 {
		t.Fatalf("additional: got %d, want >=2", len(parsed.Additional))
	}

	var foundSRV, foundA bool
	for _, r := range parsed.Additional {
		if r.Type == dnsTypeSRV {
			foundSRV = true
			if r.SrvPort != 26531 {
				t.Errorf("SRV port: got %d, want 26531", r.SrvPort)
			}
			if r.SrvTarget != "myhost.local." {
				t.Errorf("SRV target: got %q", r.SrvTarget)
			}
		}
		if r.Type == dnsTypeA {
			foundA = true
			if !r.AAddr.Equal(ip) {
				t.Errorf("A addr: got %v, want %v", r.AAddr, ip)
			}
		}
	}
	if !foundSRV {
		t.Error("no SRV record in additional")
	}
	if !foundA {
		t.Error("no A record in additional")
	}
}

func TestBuildPTRQuery(t *testing.T) {
	data, err := buildPTRQuery(mdnsService)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parsed, err := dnsMessageParse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if parsed.Header.Flags&dnsFlagQR != 0 {
		t.Error("query should not have QR flag")
	}
	if len(parsed.Questions) != 1 {
		t.Fatalf("questions: got %d", len(parsed.Questions))
	}
	if parsed.Questions[0].Name != mdnsService {
		t.Errorf("service: got %q", parsed.Questions[0].Name)
	}
}

func TestMDNSProcessResponse_ExtractPeer(t *testing.T) {
	ip := net.IP{10, 0, 0, 5}
	data, err := buildServiceResponse("Turbo Raccoon", "raccoon-pc", 26431, ip, 120)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	parsed, err := dnsMessageParse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	candidates := make(map[string]*mdnsCandidate)
	var allRecords []dnsRecord
	allRecords = append(allRecords, parsed.Answers...)
	allRecords = append(allRecords, parsed.Additional...)
	for _, r := range allRecords {
		switch r.Type {
		case dnsTypePTR:
			name := extractInstanceFromPTR(r.PtrName)
			if name != "" {
				candidates[name] = &mdnsCandidate{name: name}
			}
		case dnsTypeSRV:
			name := extractInstanceName(r.Name)
			if c, ok := candidates[name]; ok {
				c.port = r.SrvPort
			}
		case dnsTypeA:
			for _, c := range candidates {
				if c.ipv4 == nil {
					c.ipv4 = r.AAddr
				}
			}
		}
	}

	c, ok := candidates["Turbo Raccoon"]
	if !ok {
		t.Fatal("candidate not found")
	}
	if c.port != 26431 {
		t.Errorf("port: got %d", c.port)
	}
	if !c.ipv4.Equal(ip) {
		t.Errorf("ip: got %v", c.ipv4)
	}
}

func extractInstanceFromPTR(ptrName string) string {
	suffix := "." + mdnsService
	if len(ptrName) > len(suffix) && ptrName[len(ptrName)-len(suffix):] == suffix {
		return ptrName[:len(ptrName)-len(suffix)]
	}
	return ""
}

func TestMDNSProcessResponse_SkipSelf(t *testing.T) {
	ip := net.IP{10, 0, 0, 5}
	data, _ := buildServiceResponse("My Name", "myhost", 26431, ip, 120)
	parsed, _ := dnsMessageParse(data)

	for _, r := range parsed.Answers {
		if r.Type == dnsTypePTR {
			name := extractInstanceFromPTR(r.PtrName)
			if name == "My Name" {
				return // correctly would be skipped
			}
		}
	}
	t.Error("should have found self in PTR")
}

func TestMDNSProcessResponse_Goodbye(t *testing.T) {
	ip := net.IP{10, 0, 0, 5}
	data, _ := buildServiceResponse("Leaving Peer", "host", 26431, ip, 0)
	parsed, _ := dnsMessageParse(data)

	for _, r := range parsed.Answers {
		if r.Type == dnsTypePTR && r.TTL == 0 {
			return // correctly detected goodbye
		}
	}
	t.Error("should have found TTL=0 goodbye")
}

func TestMDNSProcessResponse_NonKeibidrop(t *testing.T) {
	msg := &dnsMessage{
		Header: dnsHeader{Flags: dnsFlagQR | dnsFlagAA},
		Answers: []dnsRecord{
			{
				Name:    "_http._tcp.local.",
				Type:    dnsTypePTR,
				TTL:     120,
				RData:   dnsNameEncode("webserver._http._tcp.local."),
				PtrName: "webserver._http._tcp.local.",
			},
		},
	}
	data, _ := dnsMessageMarshal(msg)
	parsed, _ := dnsMessageParse(data)

	for _, r := range parsed.Answers {
		if r.Type == dnsTypePTR {
			name := extractInstanceFromPTR(r.PtrName)
			if name != "" {
				t.Errorf("should not extract from non-keibidrop service, got %q", name)
			}
		}
	}
}

func TestHasKeibidropQuestion(t *testing.T) {
	yes := &dnsMessage{Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypePTR}}}
	if !hasKeibidropQuestion(yes) {
		t.Error("should match keibidrop PTR question")
	}

	no := &dnsMessage{Questions: []dnsQuestion{{Name: "_http._tcp.local.", Type: dnsTypePTR}}}
	if hasKeibidropQuestion(no) {
		t.Error("should not match http service")
	}

	wrongType := &dnsMessage{Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypeA}}}
	if hasKeibidropQuestion(wrongType) {
		t.Error("should not match A query for service")
	}
}

func TestMDNSHostname(t *testing.T) {
	h := mdnsHostname()
	if h == "" {
		t.Error("hostname should not be empty")
	}
	for _, r := range h {
		if r >= 'A' && r <= 'Z' {
			t.Errorf("hostname should be lowercase, got %q", h)
			break
		}
	}
}

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		ip   net.IP
		want bool
	}{
		{net.IP{192, 168, 1, 1}, true},
		{net.IP{10, 0, 0, 1}, true},
		{net.IP{172, 16, 0, 1}, true},
		{net.IP{172, 31, 255, 255}, true},
		{net.IP{8, 8, 8, 8}, false},
		{net.IP{172, 32, 0, 1}, false},
	}
	for _, tt := range tests {
		if got := isPrivateIP(tt.ip); got != tt.want {
			t.Errorf("isPrivateIP(%v) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestExtractInstanceName(t *testing.T) {
	tests := []struct {
		full string
		want string
	}{
		{"Swift Penguin._keibidrop._tcp.local.", "Swift Penguin"},
		{"_keibidrop._tcp.local.", ""},
		{"random._http._tcp.local.", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := extractInstanceName(tt.full); got != tt.want {
			t.Errorf("extractInstanceName(%q) = %q, want %q", tt.full, got, tt.want)
		}
	}
}

func TestDNSMessage_ParseRealBonjourPacket(t *testing.T) {
	// Simulated Apple Bonjour response with name compression
	var buf []byte

	// Header: QR=1, AA=1, 0 questions, 1 answer, 0 auth, 2 additional
	hdr := dnsHeader{Flags: dnsFlagQR | dnsFlagAA, ANCount: 1, ARCount: 2}
	var hdrBuf [dnsHeaderLen]byte
	hdr.marshal(hdrBuf[:])
	buf = append(buf, hdrBuf[:]...)

	// Answer: PTR record for _keibidrop._tcp.local. -> Test Device._keibidrop._tcp.local.
	serviceNameOff := len(buf)
	serviceEncoded := dnsNameEncode(mdnsService)
	buf = append(buf, serviceEncoded...)
	buf = binary.BigEndian.AppendUint16(buf, dnsTypePTR)
	buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
	buf = binary.BigEndian.AppendUint32(buf, 120)
	instanceFull := dnsNameEncode("Test Device." + mdnsService)
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(instanceFull)))
	buf = append(buf, instanceFull...)

	// Additional 1: SRV record using pointer to instance name
	instanceNameOff := len(buf)
	instanceEncoded := dnsNameEncode("Test Device." + mdnsService)
	buf = append(buf, instanceEncoded...)
	buf = binary.BigEndian.AppendUint16(buf, dnsTypeSRV)
	buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
	buf = binary.BigEndian.AppendUint32(buf, 120)
	srvRData := makeSRVRData(0, 0, 26431, "testdevice.local.")
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(srvRData)))
	buf = append(buf, srvRData...)

	// Additional 2: A record
	hostEncoded := dnsNameEncode("testdevice.local.")
	buf = append(buf, hostEncoded...)
	buf = binary.BigEndian.AppendUint16(buf, dnsTypeA)
	buf = binary.BigEndian.AppendUint16(buf, dnsClassIN)
	buf = binary.BigEndian.AppendUint32(buf, 120)
	buf = binary.BigEndian.AppendUint16(buf, 4)
	buf = append(buf, 192, 168, 1, 100)

	_ = serviceNameOff
	_ = instanceNameOff

	parsed, err := dnsMessageParse(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if len(parsed.Answers) != 1 {
		t.Fatalf("answers: got %d", len(parsed.Answers))
	}
	if parsed.Answers[0].PtrName == "" {
		t.Fatal("PTR name not parsed")
	}

	var srvPort uint16
	var aIP net.IP
	for _, r := range parsed.Additional {
		if r.Type == dnsTypeSRV {
			srvPort = r.SrvPort
		}
		if r.Type == dnsTypeA {
			aIP = r.AAddr
		}
	}
	if srvPort != 26431 {
		t.Errorf("SRV port: got %d", srvPort)
	}
	if !aIP.Equal(net.IP{192, 168, 1, 100}) {
		t.Errorf("A addr: got %v", aIP)
	}
}

func TestMakeSRVRData(t *testing.T) {
	data := makeSRVRData(0, 0, 8080, "host.local.")
	if len(data) < 6 {
		t.Fatalf("SRV rdata too short: %d", len(data))
	}
	priority := binary.BigEndian.Uint16(data[0:])
	weight := binary.BigEndian.Uint16(data[2:])
	port := binary.BigEndian.Uint16(data[4:])
	if priority != 0 || weight != 0 || port != 8080 {
		t.Errorf("SRV fields: priority=%d weight=%d port=%d", priority, weight, port)
	}
	target, _, err := dnsNameDecode(data, 6)
	if err != nil {
		t.Fatalf("decode target: %v", err)
	}
	if target != "host.local." {
		t.Errorf("target: got %q", target)
	}
}
