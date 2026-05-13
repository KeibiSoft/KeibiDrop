// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package discovery

import (
	"encoding/binary"
	"fmt"
	"net"
	"strings"
)

const (
	mdnsAddr    = "224.0.0.251:5353"
	mdnsService = "_keibidrop._tcp.local."
	mdnsDomain  = "local."

	dnsFlagQR = 0x8000
	dnsFlagAA = 0x0400

	dnsTypePTR  = 12
	dnsTypeSRV  = 33
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsClassIN  = 1

	dnsMaxNameDepth = 10
	dnsHeaderLen    = 12
)

type dnsHeader struct {
	ID      uint16
	Flags   uint16
	QDCount uint16
	ANCount uint16
	NSCount uint16
	ARCount uint16
}

func (h *dnsHeader) marshal(buf []byte) {
	binary.BigEndian.PutUint16(buf[0:], h.ID)
	binary.BigEndian.PutUint16(buf[2:], h.Flags)
	binary.BigEndian.PutUint16(buf[4:], h.QDCount)
	binary.BigEndian.PutUint16(buf[6:], h.ANCount)
	binary.BigEndian.PutUint16(buf[8:], h.NSCount)
	binary.BigEndian.PutUint16(buf[10:], h.ARCount)
}

func (h *dnsHeader) unmarshal(buf []byte) error {
	if len(buf) < dnsHeaderLen {
		return fmt.Errorf("dns header too short: %d", len(buf))
	}
	h.ID = binary.BigEndian.Uint16(buf[0:])
	h.Flags = binary.BigEndian.Uint16(buf[2:])
	h.QDCount = binary.BigEndian.Uint16(buf[4:])
	h.ANCount = binary.BigEndian.Uint16(buf[6:])
	h.NSCount = binary.BigEndian.Uint16(buf[8:])
	h.ARCount = binary.BigEndian.Uint16(buf[10:])
	return nil
}

type dnsQuestion struct {
	Name  string
	Type  uint16
	Class uint16
}

type dnsRecord struct {
	Name  string
	Type  uint16
	Class uint16
	TTL   uint32
	RData []byte

	PtrName   string
	SrvPort   uint16
	SrvTarget string
	AAddr     net.IP
}

type dnsMessage struct {
	Header     dnsHeader
	Questions  []dnsQuestion
	Answers    []dnsRecord
	Additional []dnsRecord
}

func dnsNameEncode(name string) []byte {
	if name == "." || name == "" {
		return []byte{0}
	}
	name = strings.TrimSuffix(name, ".")
	labels := strings.Split(name, ".")
	var buf []byte
	for _, label := range labels {
		if len(label) > 63 {
			label = label[:63]
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	buf = append(buf, 0)
	return buf
}

func dnsNameDecode(buf []byte, offset int) (string, int, error) {
	if offset >= len(buf) {
		return "", offset, fmt.Errorf("name offset %d past end %d", offset, len(buf))
	}
	var labels []string
	startOffset := offset
	jumped := false
	returnOffset := 0
	depth := 0

	for {
		if depth > dnsMaxNameDepth {
			return "", offset, fmt.Errorf("name pointer loop at offset %d", offset)
		}
		if offset >= len(buf) {
			return "", offset, fmt.Errorf("name truncated at offset %d", offset)
		}

		length := int(buf[offset])

		if length == 0 {
			if !jumped {
				returnOffset = offset + 1
			}
			break
		}

		if length&0xC0 == 0xC0 {
			if offset+1 >= len(buf) {
				return "", offset, fmt.Errorf("name pointer truncated at offset %d", offset)
			}
			ptr := int(binary.BigEndian.Uint16(buf[offset:offset+2]) & 0x3FFF)
			if ptr >= startOffset && !jumped {
				return "", offset, fmt.Errorf("name pointer forward reference at offset %d", offset)
			}
			if !jumped {
				returnOffset = offset + 2
				jumped = true
			}
			offset = ptr
			depth++
			continue
		}

		offset++
		end := offset + length
		if end > len(buf) {
			return "", offset, fmt.Errorf("name label truncated: need %d bytes at offset %d", length, offset)
		}
		labels = append(labels, string(buf[offset:end]))
		offset = end
	}

	return strings.Join(labels, ".") + ".", returnOffset, nil
}

func dnsMessageMarshal(msg *dnsMessage) ([]byte, error) {
	buf := make([]byte, dnsHeaderLen, 512)
	msg.Header.QDCount = uint16(len(msg.Questions))
	msg.Header.ANCount = uint16(len(msg.Answers))
	msg.Header.ARCount = uint16(len(msg.Additional))
	msg.Header.marshal(buf)

	for _, q := range msg.Questions {
		buf = append(buf, dnsNameEncode(q.Name)...)
		buf = binary.BigEndian.AppendUint16(buf, q.Type)
		buf = binary.BigEndian.AppendUint16(buf, q.Class)
	}

	appendRecords := func(records []dnsRecord) {
		for _, r := range records {
			buf = append(buf, dnsNameEncode(r.Name)...)
			buf = binary.BigEndian.AppendUint16(buf, r.Type)
			buf = binary.BigEndian.AppendUint16(buf, r.Class)
			buf = binary.BigEndian.AppendUint32(buf, r.TTL)
			buf = binary.BigEndian.AppendUint16(buf, uint16(len(r.RData)))
			buf = append(buf, r.RData...)
		}
	}

	appendRecords(msg.Answers)
	appendRecords(msg.Additional)

	return buf, nil
}

func dnsMessageParse(buf []byte) (*dnsMessage, error) {
	if len(buf) < dnsHeaderLen {
		return nil, fmt.Errorf("message too short: %d bytes", len(buf))
	}

	msg := &dnsMessage{}
	if err := msg.Header.unmarshal(buf); err != nil {
		return nil, err
	}

	offset := dnsHeaderLen

	for i := 0; i < int(msg.Header.QDCount); i++ {
		name, newOff, err := dnsNameDecode(buf, offset)
		if err != nil {
			return nil, fmt.Errorf("question %d name: %w", i, err)
		}
		offset = newOff
		if offset+4 > len(buf) {
			return nil, fmt.Errorf("question %d type/class truncated", i)
		}
		q := dnsQuestion{
			Name:  name,
			Type:  binary.BigEndian.Uint16(buf[offset:]),
			Class: binary.BigEndian.Uint16(buf[offset+2:]),
		}
		offset += 4
		msg.Questions = append(msg.Questions, q)
	}

	parseRecords := func(count int) ([]dnsRecord, error) {
		var records []dnsRecord
		for i := 0; i < count; i++ {
			name, newOff, err := dnsNameDecode(buf, offset)
			if err != nil {
				return nil, fmt.Errorf("record %d name: %w", i, err)
			}
			offset = newOff
			if offset+10 > len(buf) {
				return nil, fmt.Errorf("record %d header truncated", i)
			}
			r := dnsRecord{
				Name:  name,
				Type:  binary.BigEndian.Uint16(buf[offset:]),
				Class: binary.BigEndian.Uint16(buf[offset+2:]),
				TTL:   binary.BigEndian.Uint32(buf[offset+4:]),
			}
			rdLen := int(binary.BigEndian.Uint16(buf[offset+8:]))
			offset += 10
			if offset+rdLen > len(buf) {
				return nil, fmt.Errorf("record %d rdata truncated", i)
			}
			r.RData = buf[offset : offset+rdLen]

			switch r.Type {
			case dnsTypePTR:
				ptrName, _, err := dnsNameDecode(buf, offset)
				if err == nil {
					r.PtrName = ptrName
				}
			case dnsTypeSRV:
				if rdLen >= 6 {
					r.SrvPort = binary.BigEndian.Uint16(buf[offset+4:])
					target, _, err := dnsNameDecode(buf, offset+6)
					if err == nil {
						r.SrvTarget = target
					}
				}
			case dnsTypeA:
				if rdLen == 4 {
					r.AAddr = net.IP(buf[offset : offset+4])
				}
			case dnsTypeAAAA:
				if rdLen == 16 {
					r.AAddr = net.IP(buf[offset : offset+16])
				}
			}

			offset += rdLen
			records = append(records, r)
		}
		return records, nil
	}

	answers, err := parseRecords(int(msg.Header.ANCount))
	if err != nil {
		return nil, fmt.Errorf("answers: %w", err)
	}
	msg.Answers = answers

	additional, err := parseRecords(int(msg.Header.ARCount) + int(msg.Header.NSCount))
	if err != nil {
		return nil, fmt.Errorf("additional: %w", err)
	}
	msg.Additional = additional

	return msg, nil
}

func buildPTRQuery(service string) ([]byte, error) {
	msg := &dnsMessage{
		Questions: []dnsQuestion{{Name: service, Type: dnsTypePTR, Class: dnsClassIN}},
	}
	return dnsMessageMarshal(msg)
}

func buildServiceResponse(instanceName, hostname string, port uint16, ip net.IP, ttl uint32) ([]byte, error) {
	fullInstance := instanceName + "." + mdnsService

	ptrRData := dnsNameEncode(fullInstance)
	srvRData := makeSRVRData(0, 0, port, hostname+"."+mdnsDomain)
	aRData := ip.To4()
	if aRData == nil {
		return nil, fmt.Errorf("invalid IPv4 address")
	}

	msg := &dnsMessage{
		Header: dnsHeader{Flags: dnsFlagQR | dnsFlagAA},
		Answers: []dnsRecord{
			{Name: mdnsService, Type: dnsTypePTR, Class: dnsClassIN, TTL: ttl, RData: ptrRData},
		},
		Additional: []dnsRecord{
			{Name: fullInstance, Type: dnsTypeSRV, Class: dnsClassIN, TTL: ttl, RData: srvRData},
			{Name: hostname + "." + mdnsDomain, Type: dnsTypeA, Class: dnsClassIN, TTL: ttl, RData: aRData},
		},
	}
	return dnsMessageMarshal(msg)
}

func makeSRVRData(priority, weight, port uint16, target string) []byte {
	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, priority)
	buf = binary.BigEndian.AppendUint16(buf, weight)
	buf = binary.BigEndian.AppendUint16(buf, port)
	buf = append(buf, dnsNameEncode(target)...)
	return buf
}
