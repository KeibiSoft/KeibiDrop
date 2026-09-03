// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: MCP method handling and the tool catalogue kdmcp exposes.

package main

import (
	"encoding/json"
	"fmt"
)

type handler struct {
	kd *peer
}

type toolDef struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// obj builds a JSON Schema object node. Every tool takes an object, so this
// keeps the catalogue readable.
func obj(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func num(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

// tools is the catalogue. Descriptions are written for the agent that reads
// them, so each one states what blocks, what does not, and what it costs.
func tools() []toolDef {
	return []toolDef{
		{
			Name:  "kd_setup",
			Title: "Prepare this machine",
			Description: "Provision and inspect the local KeibiDrop environment. Creates the " +
				"config, save and mount directories if missing, reports whether FUSE is " +
				"available (on-demand mount) or absent (copy mode), and returns this peer's " +
				"code. Safe to call repeatedly. Call this first; every other tool works " +
				"without it, but this is what tells you which mode you are in.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_create_share",
			Title: "Create a share and wait for a peer",
			Description: "Return this peer's code, immediately. Give it to the other peer and " +
				"ask for theirs, then pass theirs to kd_accept_peer. KeibiDrop authenticates " +
				"both directions, so each side must know the other's code before a connection " +
				"completes, and neither side connects on one code alone. The engine decides " +
				"which peer listens and which dials by comparing the two codes, so the order " +
				"of the two calls does not matter. Idempotent: the code is this peer's " +
				"identity and never changes while the process lives.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_accept_peer",
			Title: "Accept the joining peer's code",
			Description: "Supply the other peer's code and start connecting. Returns " +
				"immediately; the connection is established in the background, so poll " +
				"kd_status until connected is true. Safe to call before the other peer has " +
				"called anything: it retries for several minutes. Idempotent.",
			InputSchema: obj(map[string]any{
				"code": str("The other peer's code, as returned by their kd_create_share, kd_join or kd_setup."),
			}, "code"),
		},
		{
			Name:  "kd_join",
			Title: "Join a peer's share",
			Description: "Join using the other peer's code. Identical to kd_accept_peer except " +
				"that it waits: it blocks up to timeout_s (default 30) for the session to " +
				"come up. The response always carries my_code, this peer's own code, which " +
				"the other side needs. If it returns connected=false the other side has not " +
				"supplied this peer's code yet, so hand over my_code and poll kd_status; the " +
				"attempt keeps retrying in the background. Idempotent.",
			InputSchema: obj(map[string]any{
				"code":      str("The creator's code."),
				"timeout_s": num("Seconds to wait for the connection. Default 30."),
			}, "code"),
		},
		{
			Name:  "kd_status",
			Title: "Session status",
			Description: "Current session state: connected or not, peer fingerprint, transport " +
				"mode, bytes moved, active transfers, and where the peer's files are readable " +
				"(mount_path when FUSE is on, save_path otherwise). Immediate. This is the " +
				"poll verb for pairing.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_list_files",
			Title: "List files in the session",
			Description: "List files this peer shares and files the peer offers. Reads the " +
				"last-known list from the session tracker; it does not trigger a sync, so a " +
				"file the peer added a moment ago may take a few seconds to appear. " +
				"Immediate.",
			InputSchema: obj(map[string]any{
				"source": str("Filter: local, remote, or all. Default all."),
			}),
		},
		{
			Name:  "kd_send_file",
			Title: "Offer a file to the peer",
			Description: "Offer a file, or a directory tree, to the connected peer. This sends " +
				"the listing, and a file's bytes move when the peer opens that file. A " +
				"directory is offered at any depth, keeping the structure; symbolic links " +
				"are left behind, because KeibiDrop carries regular files. The path must " +
				"resolve inside KD_SHARE_ROOT; if KD_SHARE_ROOT is unset, sending is " +
				"disabled and this tool refuses. Requires a live session. " +
				"This tool announces one file per call, which suits a file or a small folder. " +
				"For a large folder, point KD_SAVE_PATH at it and set " +
				"KEIBIDROP_SCAN_SHARED_ON_START=1: the engine then offers that folder in " +
				"place when the session starts, batching the announcements, which is the " +
				"path built for volume.",
			InputSchema: obj(map[string]any{
				"path":        str("Path to a file or a directory, inside KD_SHARE_ROOT."),
				"remote_name": str("Optional name the peer sees. Defaults to the base name of the path."),
			}, "path"),
		},
		{
			Name:  "kd_pull_file",
			Title: "Download a file from the peer",
			Description: "Start downloading a peer file into KD_SAVE_PATH and return a " +
				"transfer_id immediately. Poll kd_transfer_status with that id. Never blocks " +
				"for the transfer, so it is safe for files far larger than any tool-call " +
				"timeout. Resumable: re-pulling an interrupted file continues from its " +
				"chunk bitmap. With FUSE on you usually do not need this at all: read the " +
				"file straight off mount_path and only the bytes you touch cross the wire.",
			InputSchema: obj(map[string]any{
				"name": str("File name as shown by kd_list_files."),
			}, "name"),
		},
		{
			Name:  "kd_transfer_status",
			Title: "Poll a transfer",
			Description: "Progress for one transfer_id: done, bytes, total, percent, and error " +
				"if it failed. Immediate. This is the poll verb that makes large pulls " +
				"survivable.",
			InputSchema: obj(map[string]any{
				"transfer_id": str("Id returned by kd_pull_file."),
			}, "transfer_id"),
		},
		{
			Name:  "kd_credit",
			Title: "Relay credit",
			Description: "How much relay credit this peer holds, and what the current session " +
				"is using. Credit buys priority on the bridge, the relay-assisted path used " +
				"when two peers cannot reach each other directly. A peer with no credit still " +
				"transfers: bridged traffic uses the free tier, which is slower only while the " +
				"bridge is busy. A direct connection never spends credit.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_buy_credit",
			Title: "Get a link to buy a data pack",
			Description: "Return a checkout link for a data pack. A person opens the link and " +
				"pays through Stripe; this tool takes no money and needs no card details. " +
				"While this server keeps running it picks up the resulting code on its own and " +
				"adds it, and kd_wait_event then reports tokens_added.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_add_credit",
			Title: "Add a data pack code",
			Description: "Add a prepaid data pack code to this peer. A code works like cash: " +
				"whoever holds it can spend it, and a lost code cannot be recovered.",
			InputSchema: obj(map[string]any{
				"code": str("The data pack code."),
			}, "code"),
		},
		{
			Name:  "kd_peers",
			Title: "List saved peers",
			Description: "Peers saved on this machine, with each one's code and whether it is " +
				"online now. Needs a stored identity, so it reports unsupported under " +
				"KD_INCOGNITO.",
			InputSchema: obj(nil),
		},
		{
			Name:  "kd_save_peer",
			Title: "Save the connected peer",
			Description: "Store the peer of the current session under a name. After this, " +
				"kd_connect_peer reconnects to it without any code exchange. Requires a live " +
				"session and a stored identity.",
			InputSchema: obj(map[string]any{
				"name": str("The name to file this peer under."),
			}, "name"),
		},
		{
			Name:  "kd_connect_peer",
			Title: "Reconnect to a saved peer",
			Description: "Connect to a peer saved earlier, by name or by code, with no code " +
				"exchange. Waits up to timeout_s (default 30) and returns the session state. " +
				"On reconnecting to the same peer, that peer's files are listed again and a " +
				"part-finished download continues from where it stopped rather than " +
				"restarting.",
			InputSchema: obj(map[string]any{
				"name":      str("The saved name, or the peer's code."),
				"timeout_s": num("Seconds to wait for the connection. Default 30."),
			}, "name"),
		},
		{
			Name:  "kd_wait_event",
			Title: "Wait for the next session event",
			Description: "Block until the session reports something, or until timeout_s runs " +
				"out (default 30, maximum 120). This is how a headless agent follows a " +
				"session: call it, get woken when the state changes, act, call it again. " +
				"Returns event as a name, with detail when the engine supplied one, and " +
				"timed_out true when the wait ran out. The queue holds 128 events and drops " +
				"its oldest when it fills, so treat kd_status as the authoritative state and " +
				"this as the wake-up.",
			InputSchema: obj(map[string]any{
				"timeout_s": num("Seconds to wait. Default 30, maximum 120."),
			}),
		},
		{
			Name:  "kd_disconnect",
			Title: "End the session",
			Description: "Disconnect from the peer, unmount, and reset session state so a new " +
				"share can be created. Idempotent: safe when not connected.",
			InputSchema: obj(nil),
		},
	}
}

func (h *handler) dispatch(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return h.initialize(req.Params), nil

	case "notifications/initialized", "notifications/cancelled":
		return nil, nil

	case "ping":
		return map[string]any{}, nil

	case "tools/list":
		return map[string]any{"tools": tools()}, nil

	case "tools/call":
		return h.callTool(req.Params)

	default:
		return nil, &rpcError{Code: errMethodNotFound, Message: "unknown method: " + req.Method}
	}
}

func (h *handler) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)

	version := protocolVersion
	if p.ProtocolVersion != "" {
		// Echo what the client asked for. It only ever asks for a version it
		// can speak, and this server's surface is stable across the versions
		// that exist.
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo": map[string]any{
			"name":    "keibidrop",
			"version": buildVersion(),
		},
		"instructions": serverInstructions,
	}
}

const serverInstructions = `KeibiDrop moves files directly between two machines, encrypted end to end, with no cloud storage in between.

Pairing is mutual: each side must know the other's code before a session completes. There is
no trust-on-first-use, so a single code is never enough to connect.
  1. Both peers call kd_create_share (or kd_setup) to read their own code.
  2. The two codes are exchanged by whatever channel the humans or agents already share.
  3. Each peer passes the other's code to kd_accept_peer, or to kd_join to wait for the result.
  4. Both poll kd_status until connected is true.
The engine picks which peer listens and which dials by comparing the codes, so step 3 can
happen in either order, and whichever side goes first retries until the other arrives.

Once connected, prefer the mount over copying. When kd_status reports fuse true, the peer's
files are readable at mount_path with ordinary filesystem tools, and only the bytes you
actually read cross the network. Use kd_pull_file only to make a local copy, or when fuse is
false.

Sending is off unless KD_SHARE_ROOT is set, and only paths inside it can be sent.`

// callTool routes one tools/call. Tool-level failures come back as an MCP tool
// result with isError set, not as a JSON-RPC error: the agent is meant to read
// the code and branch, and a protocol error would not reach it as content.
func (h *handler) callTool(params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: errInvalidRequest, Message: "bad tools/call params"}
	}

	out, err := h.invoke(p.Name, p.Arguments)
	if err != nil {
		return toolResult(map[string]any{
			"error": map[string]any{"code": err.Code, "message": err.Message},
		}, true), nil
	}
	return toolResult(out, false), nil
}

// toolResult renders a value as an MCP tool result. Every payload is JSON in a
// text block: the schema's rule is structured JSON, never prose.
func toolResult(v any, isError bool) map[string]any {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		b = []byte(fmt.Sprintf(`{"error":{"code":"internal","message":%q}}`, err.Error()))
		isError = true
	}
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(b)}},
		"isError":           isError,
		"structuredContent": v,
	}
}
