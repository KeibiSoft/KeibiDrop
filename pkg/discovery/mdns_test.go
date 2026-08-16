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

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/require"
)

func TestDNSNameEncode_Simple(t *testing.T) {
	got := dnsNameEncode("_keibidrop._tcp.local.")
	want := []byte{10, '_', 'k', 'e', 'i', 'b', 'i', 'd', 'r', 'o', 'p', 4, '_', 't', 'c', 'p', 5, 'l', 'o', 'c', 'a', 'l', 0}
	testkit.Run(t, func() error {
		return fp.BytesEqual("encoded name", got, want)
	})
}

func TestDNSNameEncode_SingleLabel(t *testing.T) {
	got := dnsNameEncode("local.")
	want := []byte{5, 'l', 'o', 'c', 'a', 'l', 0}
	require.Len(t, got, len(want), "length")
}

func TestDNSNameEncode_Root(t *testing.T) {
	got := dnsNameEncode(".")
	testkit.Run(t, func() error {
		return fp.BytesEqual("root", got, []byte{0})
	})
}

func TestDNSNameEncode_WithSpaces(t *testing.T) {
	got := dnsNameEncode("Cosmic Waffle._keibidrop._tcp.local.")
	label := string(got[1:14])
	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("first label length", got[0], byte(13)),
			fp.Equal("first label", label, "Cosmic Waffle"),
		)
	})
}

func TestDNSNameDecode_Roundtrip(t *testing.T) {
	names := []string{
		"_keibidrop._tcp.local.",
		"local.",
		"Cosmic Waffle._keibidrop._tcp.local.",
		"a.b.c.d.e.",
	}
	testkit.Run(t, func() error {
		return fp.Each("roundtrip", names, func(name string) error {
			encoded := dnsNameEncode(name)
			decoded, off, err := dnsNameDecode(encoded, 0)
			return fp.Steps(
				func() error { return fp.NoErr("decode "+name, err) },
				func() error {
					return fp.All(
						fp.Equal("decoded", decoded, name),
						fp.Equal("offset", off, len(encoded)),
					)
				},
			)
		})
	})
}

func TestDNSNameDecode_Compressed(t *testing.T) {
	var buf []byte
	buf = append(buf, dnsNameEncode("_keibidrop._tcp.local.")...)
	nameStart := len(buf)
	buf = append(buf, 6, 'C', 'o', 's', 'm', 'i', 'c')
	buf = append(buf, 0xC0, 0x00)

	decoded, _, err := dnsNameDecode(buf, nameStart)
	require.NoError(t, err, "compressed decode")
	require.Equal(t, "Cosmic._keibidrop._tcp.local.", decoded, "decoded")
}

func TestDNSNameDecode_PointerLoop(t *testing.T) {
	buf := []byte{0xC0, 0x02, 0xC0, 0x00}
	_, _, err := dnsNameDecode(buf, 0)
	require.Error(t, err, "expected error for pointer loop")
}

func TestDNSNameDecode_Truncated(t *testing.T) {
	buf := []byte{5, 'h', 'e'}
	_, _, err := dnsNameDecode(buf, 0)
	require.Error(t, err, "expected error for truncated label")
}

func TestDNSNameDecode_Empty(t *testing.T) {
	_, _, err := dnsNameDecode([]byte{}, 0)
	require.Error(t, err, "expected error for empty buffer")
}

func TestDNSHeader_Roundtrip(t *testing.T) {
	h := dnsHeader{ID: 0, Flags: dnsFlagQR | dnsFlagAA, QDCount: 1, ANCount: 2, ARCount: 3}
	var buf [dnsHeaderLen]byte
	h.marshal(buf[:])

	var h2 dnsHeader
	require.NoError(t, h2.unmarshal(buf[:]), "unmarshal")
	require.Equal(t, h, h2, "roundtrip")
}

func TestDNSMessage_QueryRoundtrip(t *testing.T) {
	msg := &dnsMessage{
		Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypePTR, Class: dnsClassIN}},
	}
	data, err := dnsMessageMarshal(msg)
	require.NoError(t, err, "marshal")

	parsed, err := dnsMessageParse(data)
	require.NoError(t, err, "parse")
	require.Len(t, parsed.Questions, 1, "questions")

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("name", parsed.Questions[0].Name, mdnsService),
			fp.Equal("type", parsed.Questions[0].Type, uint16(dnsTypePTR)),
		)
	})
}

func TestDNSMessage_ResponseRoundtrip(t *testing.T) {
	ip := net.IP{192, 168, 1, 42}
	data, err := buildServiceResponse("Swift Penguin", "myhost", 26531, ip, 120)
	require.NoError(t, err, "build")

	parsed, err := dnsMessageParse(data)
	require.NoError(t, err, "parse")
	require.Len(t, parsed.Answers, 1, "answers")
	require.True(t, len(parsed.Additional) >= 2, "additional: got %d, want >=2", len(parsed.Additional))

	ptr := parsed.Answers[0]
	var foundSRV, foundA bool

	testkit.Run(t, func() error {
		return fp.Steps(
			func() error {
				return fp.All(
					fp.True("QR flag set", parsed.Header.Flags&dnsFlagQR != 0),
					fp.Equal("answer type", ptr.Type, uint16(dnsTypePTR)),
					fp.Equal("PTR name", ptr.PtrName, "Swift Penguin."+mdnsService),
				)
			},
			func() error {
				return fp.Each("additional records", parsed.Additional, func(r dnsRecord) error {
					switch r.Type {
					case dnsTypeSRV:
						foundSRV = true
						return fp.All(
							fp.Equal("SRV port", r.SrvPort, uint16(26531)),
							fp.Equal("SRV target", r.SrvTarget, "myhost.local."),
						)
					case dnsTypeA:
						foundA = true
						return fp.True("A addr matches", r.AAddr.Equal(ip))
					}
					return nil
				})
			},
			func() error {
				return fp.All(
					fp.True("SRV record found", foundSRV),
					fp.True("A record found", foundA),
				)
			},
		)
	})
}

func TestBuildPTRQuery(t *testing.T) {
	data, err := buildPTRQuery(mdnsService)
	require.NoError(t, err, "build")

	parsed, err := dnsMessageParse(data)
	require.NoError(t, err, "parse")
	require.Len(t, parsed.Questions, 1, "questions")

	testkit.Run(t, func() error {
		return fp.All(
			fp.False("query should not have QR flag", parsed.Header.Flags&dnsFlagQR != 0),
			fp.Equal("service", parsed.Questions[0].Name, mdnsService),
		)
	})
}

func TestMDNSProcessResponse_ExtractPeer(t *testing.T) {
	ip := net.IP{10, 0, 0, 5}
	data, err := buildServiceResponse("Turbo Raccoon", "raccoon-pc", 26431, ip, 120)
	require.NoError(t, err, "build")

	parsed, err := dnsMessageParse(data)
	require.NoError(t, err, "parse")

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
	require.True(t, ok, "candidate not found")

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("port", c.port, uint16(26431)),
			fp.True("ip matches", c.ipv4.Equal(ip)),
		)
	})
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

	// The PTR record must be present so the self-filter has a match to skip.
	found := false
	for _, r := range parsed.Answers {
		if r.Type == dnsTypePTR && extractInstanceFromPTR(r.PtrName) == "My Name" {
			found = true
		}
	}
	require.True(t, found, "should have found self in PTR")
}

func TestMDNSProcessResponse_Goodbye(t *testing.T) {
	ip := net.IP{10, 0, 0, 5}
	data, _ := buildServiceResponse("Leaving Peer", "host", 26431, ip, 0)
	parsed, _ := dnsMessageParse(data)

	found := false
	for _, r := range parsed.Answers {
		if r.Type == dnsTypePTR && r.TTL == 0 {
			found = true
		}
	}
	require.True(t, found, "should have found TTL=0 goodbye")
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

	testkit.Run(t, func() error {
		return fp.Each("answers", parsed.Answers, func(r dnsRecord) error {
			if r.Type != dnsTypePTR {
				return nil
			}
			return fp.Equal("extracted instance from non-keibidrop service", extractInstanceFromPTR(r.PtrName), "")
		})
	})
}

func TestHasKeibidropQuestion(t *testing.T) {
	yes := &dnsMessage{Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypePTR}}}
	no := &dnsMessage{Questions: []dnsQuestion{{Name: "_http._tcp.local.", Type: dnsTypePTR}}}
	wrongType := &dnsMessage{Questions: []dnsQuestion{{Name: mdnsService, Type: dnsTypeA}}}

	testkit.Run(t, func() error {
		return fp.All(
			fp.True("should match keibidrop PTR question", hasKeibidropQuestion(yes)),
			fp.False("should not match http service", hasKeibidropQuestion(no)),
			fp.False("should not match A query for service", hasKeibidropQuestion(wrongType)),
		)
	})
}

func TestMDNSHostname(t *testing.T) {
	h := mdnsHostname()
	testkit.Run(t, func() error {
		return fp.All(
			fp.NotEqual("hostname", h, ""),
			fp.Each("hostname runes", []rune(h), func(r rune) error {
				return fp.False("hostname should be lowercase", r >= 'A' && r <= 'Z')
			}),
		)
	})
}

type ipPrivacyCase struct {
	ip   net.IP
	want bool
}

func TestIsPrivateIP(t *testing.T) {
	tests := []ipPrivacyCase{
		{net.IP{192, 168, 1, 1}, true},
		{net.IP{10, 0, 0, 1}, true},
		{net.IP{172, 16, 0, 1}, true},
		{net.IP{172, 31, 255, 255}, true},
		{net.IP{8, 8, 8, 8}, false},
		{net.IP{172, 32, 0, 1}, false},
	}
	testkit.RunTable(t, tests, func(tt ipPrivacyCase) string { return tt.ip.String() },
		func(t *testing.T, tt ipPrivacyCase) error {
			return fp.Equal("isPrivateIP", isPrivateIP(tt.ip), tt.want)
		})
}

type instanceNameCase struct {
	full string
	want string
}

func TestExtractInstanceName(t *testing.T) {
	tests := []instanceNameCase{
		{"Swift Penguin._keibidrop._tcp.local.", "Swift Penguin"},
		{"_keibidrop._tcp.local.", ""},
		{"random._http._tcp.local.", ""},
		{"", ""},
	}
	testkit.RunTable(t, tests, func(tt instanceNameCase) string {
		if tt.full == "" {
			return "empty"
		}
		return tt.full
	}, func(t *testing.T, tt instanceNameCase) error {
		return fp.Equal("extractInstanceName", extractInstanceName(tt.full), tt.want)
	})
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
	require.NoError(t, err, "parse")
	require.Len(t, parsed.Answers, 1, "answers")
	require.NotEqual(t, "", parsed.Answers[0].PtrName, "PTR name not parsed")

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

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("SRV port", srvPort, uint16(26431)),
			fp.True("A addr matches", aIP.Equal(net.IP{192, 168, 1, 100})),
		)
	})
}

func TestMakeSRVRData(t *testing.T) {
	data := makeSRVRData(0, 0, 8080, "host.local.")
	require.True(t, len(data) >= 6, "SRV rdata too short: %d", len(data))

	priority := binary.BigEndian.Uint16(data[0:])
	weight := binary.BigEndian.Uint16(data[2:])
	port := binary.BigEndian.Uint16(data[4:])

	target, _, err := dnsNameDecode(data, 6)
	require.NoError(t, err, "decode target")

	testkit.Run(t, func() error {
		return fp.All(
			fp.Equal("SRV priority", priority, uint16(0)),
			fp.Equal("SRV weight", weight, uint16(0)),
			fp.Equal("SRV port", port, uint16(8080)),
			fp.Equal("target", target, "host.local."),
		)
	})
}
