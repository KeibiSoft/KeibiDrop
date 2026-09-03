// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/pkg/logic/common"
)

// The exit codes are a public contract: a shell script branches on the number,
// so a change here breaks callers silently. Pinned deliberately.
func TestExitCodeContract(t *testing.T) {
	want := map[string]int{
		"":                             exitOK,
		codeOK:                         exitOK,
		codeInternal:                   exitInternal,
		codeNotConnected:               exitNotConnected,
		codeTimeout:                    exitTimeout,
		codeNotFound:                   exitNotFound,
		codeInvalidArgument:            exitInvalidArgument,
		codeBusy:                       exitBusy,
		codeRefused:                    exitRefused,
		codeUnsupported:                exitUnsupported,
		"a code from a future version": exitInternal,
	}
	for code, expected := range want {
		if got := exitCodeFor(code); got != expected {
			t.Errorf("exitCodeFor(%q) = %d, want %d", code, got, expected)
		}
	}

	// Success must be the only zero: anything else would make a failure look
	// like a success to a script that only checks $?.
	for code, exit := range want {
		if exit == 0 && code != "" && code != codeOK {
			t.Errorf("code %q maps to exit 0, which hides a failure", code)
		}
	}
}

func TestClassifyErrorSentinels(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{common.ErrInvalidSession, codeNotConnected},
		{common.ErrSessionNotEstablished, codeNotConnected},
		{common.ErrNilPointer, codeNotConnected},
		{common.ErrTimeoutReached, codeTimeout},
		{common.ErrNotFound, codeNotFound},
		{common.ErrAlreadyRunning, codeBusy},
		{common.ErrFingerprintMismatch, codeRefused},
		{common.ErrIdenticalFingerprints, codeRefused},
		{common.ErrInvalidLength, codeInvalidArgument},
		{common.ErrInvalidFingerprint, codeInvalidArgument},
		{nil, codeOK},
	}
	for _, tc := range cases {
		if got := classifyError(tc.err); got != tc.want {
			t.Errorf("classifyError(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}

	// A wrapped sentinel must classify the same as the bare one: the engine
	// wraps errors on several paths.
	wrapped := fmt.Errorf("pulling artifact.bin: %w", common.ErrTimeoutReached)
	if got := classifyError(wrapped); got != codeTimeout {
		t.Errorf("wrapped timeout classified as %s, want %s", got, codeTimeout)
	}
}

func TestClassifyMessage(t *testing.T) {
	cases := map[string]string{
		"file not found: x":                 codeNotFound,
		`file "nothing" not shared`:         codeNotFound,
		`no active download for "x"`:        codeNotFound,
		"create/join already in progress":   codeBusy,
		"timeout reached":                   codeTimeout,
		"unknown command: bogus":            codeUnsupported,
		"unknown show target: bogus":        codeUnsupported,
		"usage: kd add-contact <name> <fp>": codeInvalidArgument,
		"invalid length":                    codeInvalidArgument,
		"fingerprint mismatch":              codeRefused,
		"daemon not running":                codeNotConnected,
		"something entirely new":            codeInternal,
	}
	for msg, want := range cases {
		if got := classifyMessage(msg); got != want {
			t.Errorf("classifyMessage(%q) = %s, want %s", msg, got, want)
		}
	}
}

// "invalid session" contains both "invalid" and "session". It must read as a
// missing session, not a bad argument: an agent reconnects for one and fixes
// its call for the other. Regression pin for an arm-ordering bug.
func TestInvalidSessionIsNotAnArgumentError(t *testing.T) {
	if got := classifyMessage("invalid session"); got != codeNotConnected {
		t.Fatalf("got %s, want %s", got, codeNotConnected)
	}
	if got := exitCodeFor(classifyMessage("invalid session")); got != exitNotConnected {
		t.Fatalf("exit %d, want %d", got, exitNotConnected)
	}
}

// errResponse is what every existing dispatch arm already calls. It must keep
// the exact Error text it always produced and only add a code.
func TestErrResponseIsAdditive(t *testing.T) {
	r := errResponse("file not found: x")
	if r.OK {
		t.Error("errResponse must not be OK")
	}
	if r.Error != "file not found: x" {
		t.Errorf("error text changed: %q", r.Error)
	}
	if r.Code != codeNotFound {
		t.Errorf("code = %q, want %q", r.Code, codeNotFound)
	}

	// A success response must not carry a code at all, so existing parsers see
	// exactly the bytes they saw before.
	b, err := json.Marshal(okResponse(map[string]string{"a": "b"}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, present := m["code"]; present {
		t.Errorf("success response must omit code, got %s", b)
	}
}

func TestErrResponseForUsesSentinel(t *testing.T) {
	r := errResponseFor(errors.New("no active download for \"x\""))
	if r.Code != codeNotFound {
		t.Errorf("code = %q, want %q", r.Code, codeNotFound)
	}
	r = errResponseFor(common.ErrTimeoutReached)
	if r.Code != codeTimeout {
		t.Errorf("code = %q, want %q", r.Code, codeTimeout)
	}
}

func TestExtractTimeoutFlag(t *testing.T) {
	cases := []struct {
		in       []string
		wantArgs []string
		wantSecs int
	}{
		{nil, []string{}, 0},
		{[]string{"file.txt"}, []string{"file.txt"}, 0},
		{[]string{"--timeout=30"}, []string{}, 30},
		{[]string{"--timeout", "45"}, []string{}, 45},
		{[]string{"file.txt", "--timeout=5"}, []string{"file.txt"}, 5},
		{[]string{"--timeout=5", "file.txt"}, []string{"file.txt"}, 5},
		{[]string{"a", "--timeout", "7", "b"}, []string{"a", "b"}, 7},
		// Nonsense values leave the default in place rather than becoming zero
		// or a negative deadline.
		{[]string{"--timeout=abc"}, []string{}, 0},
		{[]string{"--timeout=-1"}, []string{}, 0},
		{[]string{"--timeout=0"}, []string{}, 0},
		// A trailing --timeout with no value must not eat the next arg or panic.
		{[]string{"file.txt", "--timeout"}, []string{"file.txt", "--timeout"}, 0},
	}
	for _, tc := range cases {
		gotArgs, gotSecs := extractTimeoutFlag(tc.in)
		if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
			t.Errorf("extractTimeoutFlag(%v) args = %v, want %v", tc.in, gotArgs, tc.wantArgs)
		}
		if gotSecs != tc.wantSecs {
			t.Errorf("extractTimeoutFlag(%v) secs = %d, want %d", tc.in, gotSecs, tc.wantSecs)
		}
	}
}

func TestTimeoutOrDefault(t *testing.T) {
	if got := timeoutOrDefault(Request{}, defaultConnectTimeout); got != defaultConnectTimeout {
		t.Errorf("zero timeout should fall back to the default, got %s", got)
	}
	if got := timeoutOrDefault(Request{TimeoutS: 5}, defaultConnectTimeout); got.Seconds() != 5 {
		t.Errorf("explicit timeout ignored, got %s", got)
	}
	if got := timeoutOrDefault(Request{TimeoutS: -3}, defaultConnectTimeout); got != defaultConnectTimeout {
		t.Errorf("negative timeout should fall back to the default, got %s", got)
	}
}

// The daemon must keep accepting a request that omits the new fields, because
// an older client is still a valid client.
func TestRequestBackCompat(t *testing.T) {
	var req Request
	if err := json.Unmarshal([]byte(`{"command":"status","args":["x"]}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.Command != "status" || len(req.Args) != 1 || req.TimeoutS != 0 {
		t.Fatalf("old-shape request parsed wrong: %+v", req)
	}
}

func TestNewTransferIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newTransferID()
		if seen[id] {
			t.Fatalf("duplicate transfer id %s", id)
		}
		seen[id] = true
	}
}
