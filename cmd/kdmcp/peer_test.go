// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newTestPeer builds a peer with only the fields the containment checks read.
// It deliberately does not start the engine: these are the rules that must
// hold before any session exists.
func newTestPeer(t *testing.T, root string) *peer {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	return &peer{shareRoot: resolved, transfers: map[string]*transfer{}}
}

func TestSafeSendPathContainment(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	inside := filepath.Join(root, "ok.txt")
	if err := os.WriteFile(inside, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "sub", "deep.txt")
	if err := os.WriteFile(nested, []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A symlink that lives inside the root but points outside it. Resolving
	// before the containment check is the whole point: a naive prefix test on
	// the unresolved path would accept this.
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(secret, escape); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	p := newTestPeer(t, root)

	tests := []struct {
		name     string
		input    string
		wantCode string // empty means the path must be accepted
	}{
		{"file inside the root", inside, ""},
		{"relative name inside the root", "ok.txt", ""},
		{"nested file", nested, ""},
		{"relative nested name", filepath.Join("sub", "deep.txt"), ""},
		{"absolute path outside the root", secret, codeForbidden},
		{"symlink inside pointing outside", escape, codeForbidden},
		{"traversal out of the root", filepath.Join("..", filepath.Base(outside), "secret.txt"), codeForbidden},
		{"the root itself is a directory", root, codeInvalidArgument},
		{"a subdirectory", filepath.Join(root, "sub"), codeInvalidArgument},
		{"missing file", filepath.Join(root, "nope.txt"), codeNotFound},
		{"empty path", "", codeInvalidArgument},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, terr := p.safeSendPath(tc.input)
			if tc.wantCode == "" {
				if terr != nil {
					t.Fatalf("expected acceptance, got %s: %s", terr.Code, terr.Message)
				}
				if !strings.HasPrefix(got, p.shareRoot) {
					t.Fatalf("accepted path %q escapes root %q", got, p.shareRoot)
				}
				return
			}
			if terr == nil {
				t.Fatalf("expected refusal with %s, got path %q", tc.wantCode, got)
			}
			if terr.Code != tc.wantCode {
				t.Fatalf("expected code %s, got %s (%s)", tc.wantCode, terr.Code, terr.Message)
			}
		})
	}
}

// Sending is disabled entirely when no share root is configured. That is the
// default, and it is what keeps a prompt-injected agent from exfiltrating.
func TestSafeSendPathDisabledWithoutRoot(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &peer{shareRoot: "", transfers: map[string]*transfer{}}

	for _, in := range []string{f, "f.txt", "", "/etc/hosts"} {
		if _, terr := p.safeSendPath(in); terr == nil || terr.Code != codeForbidden {
			t.Fatalf("input %q: expected forbidden, got %+v", in, terr)
		}
	}
}

// A root that is itself a symlink must still accept files under it. Common on
// macOS, where /tmp is a link to /private/tmp.
func TestSafeSendPathSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f := filepath.Join(real, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := newTestPeer(t, link)
	if _, terr := p.safeSendPath("f.txt"); terr != nil {
		t.Fatalf("file under a symlinked root refused: %s %s", terr.Code, terr.Message)
	}
}

// A directory is accepted as a send target, because a whole tree can be
// offered in one call. safeSendPath, which sends one file, still refuses it.
func TestResolveSendTargetAcceptsDirectories(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(sub, "deep"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "deep", "f.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newTestPeer(t, root)

	got, isDir, terr := p.resolveSendTarget("tree")
	if terr != nil {
		t.Fatalf("directory refused: %s %s", terr.Code, terr.Message)
	}
	if !isDir {
		t.Fatalf("%s should report as a directory", got)
	}

	if _, terr := p.safeSendPath("tree"); terr == nil || terr.Code != codeInvalidArgument {
		t.Fatalf("safeSendPath should refuse a directory, got %+v", terr)
	}

	// A directory outside the root is refused the same way a file is.
	if _, _, terr := p.resolveSendTarget(t.TempDir()); terr == nil || terr.Code != codeForbidden {
		t.Fatalf("a directory outside the root should be forbidden, got %+v", terr)
	}
}

func TestInsideRoot(t *testing.T) {
	p := &peer{shareRoot: filepath.Join("/srv", "share")}
	inside := []string{
		filepath.Join("/srv", "share"),
		filepath.Join("/srv", "share", "a.txt"),
		filepath.Join("/srv", "share", "deep", "b.txt"),
	}
	for _, path := range inside {
		if !p.insideRoot(path) {
			t.Errorf("%s should be inside the root", path)
		}
	}
	// "/srv/shared" starts with the root string but is a different directory.
	outside := []string{"/srv", "/srv/shared/x", "/etc/passwd", "/srv/share2"}
	for _, path := range outside {
		if p.insideRoot(path) {
			t.Errorf("%s should be outside the root", path)
		}
	}
}

func TestSafeName(t *testing.T) {
	refused := []string{
		"",
		"../escape.txt",
		"../../etc/passwd",
		"a/../../b",
		"/etc/passwd",
		"/absolute.txt",
	}
	for _, n := range refused {
		if terr := safeName(n); terr == nil {
			t.Fatalf("name %q should have been refused", n)
		}
	}

	accepted := []string{"file.txt", "sub/file.txt", "with space.bin", "dot.in.name"}
	for _, n := range accepted {
		if terr := safeName(n); terr != nil {
			t.Fatalf("name %q refused: %s", n, terr.Message)
		}
	}

	if runtime.GOOS == "windows" {
		if terr := safeName(`C:\evil.txt`); terr == nil {
			t.Fatal("a Windows absolute path should have been refused")
		}
	}
}

func TestClassifyText(t *testing.T) {
	cases := map[string]string{
		"file not found: x":                codeNotFound,
		"no such file or directory":        codeNotFound,
		"timeout reached":                  codeTimeout,
		"context deadline exceeded":        codeTimeout,
		"create/join already in progress":  codeBusy,
		"already running":                  codeBusy,
		"invalid session":                  codeNotConnected,
		"fingerprint mismatch":             codeRefused,
		"invalid length":                   codeInvalidArgument,
		"something nobody has seen before": codeInternal,
	}
	for msg, want := range cases {
		if got := classifyText(msg); got != want {
			t.Errorf("classifyText(%q) = %s, want %s", msg, got, want)
		}
	}
}

// "invalid session" contains "invalid", so arm order in classifyText decides
// whether it reads as a bad argument or a missing session. An agent retries
// those two differently, so this is pinned.
func TestClassifyTextInvalidSessionBeatsInvalid(t *testing.T) {
	if got := classifyText("invalid session"); got != codeNotConnected {
		t.Fatalf("invalid session classified as %s, want %s", got, codeNotConnected)
	}
}

// Every tool must declare a schema the MCP client can render, and every tool
// the catalogue advertises must be routable.
func TestToolCatalogue(t *testing.T) {
	list := tools()
	if len(list) == 0 {
		t.Fatal("no tools advertised")
	}
	seen := map[string]bool{}
	for _, tool := range list {
		if seen[tool.Name] {
			t.Errorf("duplicate tool %s", tool.Name)
		}
		seen[tool.Name] = true

		if !strings.HasPrefix(tool.Name, "kd_") {
			t.Errorf("tool %s should be namespaced kd_", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
		if tool.InputSchema["type"] != "object" {
			t.Errorf("tool %s input schema is not an object", tool.Name)
		}
		if _, err := json.Marshal(tool); err != nil {
			t.Errorf("tool %s does not marshal: %v", tool.Name, err)
		}
	}

	for _, required := range []string{
		"kd_create_share", "kd_join", "kd_status", "kd_list_files",
		"kd_send_file", "kd_pull_file", "kd_transfer_status", "kd_disconnect",
	} {
		if !seen[required] {
			t.Errorf("schema requires tool %s, which is missing", required)
		}
	}
}

// An unknown tool name must come back as a refusal, not a panic.
func TestInvokeUnknownTool(t *testing.T) {
	h := &handler{kd: &peer{transfers: map[string]*transfer{}}}
	_, terr := h.invoke("kd_not_a_tool", json.RawMessage(`{}`))
	if terr == nil || terr.Code != codeUnsupported {
		t.Fatalf("expected unsupported, got %+v", terr)
	}
}

// Malformed arguments must not panic. The tools unmarshal best-effort and then
// validate, so a nil, empty or syntactically broken payload has to land on a
// clean refusal rather than a crash.
func TestToolsTolerateBadArguments(t *testing.T) {
	p := &peer{transfers: map[string]*transfer{}}
	for _, args := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`not json`)} {
		if _, terr := p.transferStatus(args); terr == nil || terr.Code != codeNotFound {
			t.Errorf("transferStatus(%s) should refuse an unknown id, got %+v", args, terr)
		}
	}

	// A source the server does not know is rejected before any session state
	// is touched, rather than silently listing everything.
	if _, terr := p.listFiles(json.RawMessage(`{"source":"sideways"}`)); terr == nil {
		t.Error("listFiles should refuse an unknown source")
	}
}

func TestToolResultShape(t *testing.T) {
	res := toolResult(map[string]any{"ok": true}, false)
	content, ok := res["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content is not a single block: %#v", res["content"])
	}
	block, ok := content[0].(map[string]any)
	if !ok || block["type"] != "text" {
		t.Fatalf("block is not a text block: %#v", content[0])
	}
	// The payload must be JSON, never prose: the schema's rule.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(block["text"].(string)), &parsed); err != nil {
		t.Fatalf("tool result text is not JSON: %v", err)
	}
	if res["isError"] != false {
		t.Errorf("isError should be false")
	}
}
