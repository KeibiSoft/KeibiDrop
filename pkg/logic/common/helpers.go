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
)

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
