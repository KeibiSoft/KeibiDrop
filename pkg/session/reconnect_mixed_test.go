// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package session

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// The mixed shape opens the same legs a fresh create and join open: the responder
// (joiner) dials the peer directly first and takes pair2 as its inbound; the
// initiator (creator) accepts the direct leg first and dials pair2 as its outbound.
// Neither side ever touches pair1. The stubs refuse, so the error names the first leg.
func TestReconnectMixed_LegsMatchCreateAndJoin(t *testing.T) {
	cases := []struct {
		role      string
		initiator bool
		firstLeg  string
	}{
		{"initiator (creator): accepts the direct leg first", true, "accept direct leg"},
		{"responder (joiner): dials the peer directly first", false, "direct outbound"},
	}
	for _, tc := range cases {
		t.Run(tc.role, func(t *testing.T) {
			r := newPolicyTestManager()
			r.session = &Session{logger: testkit.DiscardLogger()}
			r.BridgeAddr = "b:26600"
			var dirs []string
			r.DialBridge = func(dir string) (net.Conn, error) {
				dirs = append(dirs, dir)
				return nil, fmt.Errorf("refused in test")
			}
			r.AcceptConn = func(timeout time.Duration) (net.Conn, error) { return nil, fmt.Errorf("nobody in test") }
			err := r.reconnectMixed(testkit.DiscardLogger(), tc.initiator)
			if err == nil {
				t.Fatal("expected the stubs to fail the attempt")
			}
			if !strings.Contains(err.Error(), tc.firstLeg) {
				t.Fatalf("first leg = %q, want it to be %q", err.Error(), tc.firstLeg)
			}
			for _, d := range dirs {
				if d == "pair1" {
					t.Fatal("the mixed shape never opens pair1")
				}
			}
		})
	}
}

// A mixed session tries its own shape before the bridge, and a failed attempt flips
// the outage to bridge-first like a failed direct attempt does.
func TestMixedFirst_OrdersTransports(t *testing.T) {
	dial := func(string) (net.Conn, error) { return nil, fmt.Errorf("test dial") }
	r := newPolicyTestManager()
	r.session = &Session{logger: testkit.DiscardLogger()}
	r.BridgeAddr = "b:26600"
	r.DialBridge = dial
	r.PreferDirect = func() bool { return false } // a blocked inbound says bridge
	r.MixedLegs = func() bool { return true }

	if !r.mixedFirst() || r.useBridgeFirst() {
		t.Fatal("a mixed session must try its shape before the bridge")
	}
	if err := r.noteDirectFailure(testkit.DiscardLogger(), fmt.Errorf("peer away")); err == nil {
		t.Fatal("the failure is returned to the loop")
	}
	if r.mixedFirst() || !r.useBridgeFirst() {
		t.Fatal("after a failed mixed attempt the outage is bridge-first")
	}

	r2 := newPolicyTestManager()
	r2.BridgeAddr = "b:26600"
	r2.DialBridge = dial
	r2.MixedLegs = func() bool { return false }
	r2.PreferDirect = func() bool { return true }
	if r2.mixedFirst() || r2.useBridgeFirst() {
		t.Fatal("a plain direct session is unchanged: direct first")
	}
}

func TestLastTransport_RecordsOnlySuccess(t *testing.T) {
	r := newPolicyTestManager()
	if r.LastTransport() != "" {
		t.Fatal("nothing reconnected yet")
	}
	_ = r.finishTransport(transportMixed, fmt.Errorf("failed"))
	if r.LastTransport() != "" {
		t.Fatal("a failed attempt records nothing")
	}
	_ = r.finishTransport(transportBridge, nil)
	if r.LastTransport() != transportBridge {
		t.Fatalf("LastTransport = %q, want bridge", r.LastTransport())
	}
}
