// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Minimal MCP stdio transport: newline-delimited JSON-RPC 2.0.
// ABOUTME: Hand-rolled on the standard library so kdmcp adds no dependency.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// The MCP surface kdmcp needs is four methods wide: initialize, tools/list,
// tools/call, ping. That is small enough that an SDK would cost more in
// supply-chain surface than it saves in code, so this file is the whole
// transport. KeibiDrop ships zero third-party code in this binary's own layer.

// protocolVersion is what we advertise when a client asks for something we do
// not recognize. MCP clients accept a server pinning a version they know.
const protocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// JSON-RPC 2.0 reserved codes.
const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInternal       = -32603
)

// conn writes one JSON object per line to stdout. Stdout is the protocol
// channel and nothing else may touch it: every log line in this process goes
// to stderr, or the client sees corrupt frames.
type conn struct {
	mu  sync.Mutex
	out io.Writer
}

func (c *conn) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = fmt.Fprintf(c.out, "%s\n", b)
}

func (c *conn) reply(id json.RawMessage, result any) {
	c.send(rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (c *conn) replyError(id json.RawMessage, code int, msg string) {
	c.send(rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// serve reads requests until stdin closes. Requests are handled one at a time:
// the engine's own verbs are either immediate or explicitly async (a pull
// returns a transfer id), so nothing here blocks long enough to need
// pipelining, and serial handling keeps session state race-free.
func serve(in io.Reader, out io.Writer, h *handler) error {
	c := &conn{out: out}
	sc := bufio.NewScanner(in)
	// MCP messages carry tool schemas and file listings; the 64 KB default is
	// not enough.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			c.replyError(nil, errParse, "invalid JSON")
			continue
		}
		// A request with no id is a notification: act on it, never answer it.
		isNotification := len(req.ID) == 0
		result, rerr := h.dispatch(req)
		if isNotification {
			continue
		}
		if rerr != nil {
			c.replyError(req.ID, rerr.Code, rerr.Message)
			continue
		}
		c.reply(req.ID, result)
	}
	return sc.Err()
}
