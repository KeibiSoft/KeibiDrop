// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: The bridge room directions of a reconnect match a fresh create and join:
// ABOUTME: the lower fingerprint reads pair1 and writes pair2, the other side the reverse.

package session

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// A peer that restarted mid-outage comes back through a fresh create or join,
// where the creator (lower fingerprint) takes pair1 as its inbound leg. The
// reconnecting side must use the same mapping, or both sides read pair1 and
// wait each other out. The stub dialer refuses the first room, so the error
// names the leg each role opens first.
func TestReconnectBridge_DirectionsMatchCreateAndJoin(t *testing.T) {
	cases := []struct {
		role      string
		initiator bool
		firstDir  string
		firstLeg  string
	}{
		{"initiator (lower fingerprint, creator in a fresh connect)", true, "pair1", "bridge dial (inbound)"},
		{"responder (higher fingerprint, joiner in a fresh connect)", false, "pair1", "bridge dial (outbound)"},
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
			err := r.reconnectBridge(testkit.DiscardLogger(), tc.initiator)
			if err == nil {
				t.Fatal("expected the stub dialer to fail the attempt")
			}
			if len(dirs) != 1 || dirs[0] != tc.firstDir {
				t.Fatalf("first room dialed = %v, want [%s]", dirs, tc.firstDir)
			}
			if !strings.Contains(err.Error(), tc.firstLeg) {
				t.Fatalf("first leg = %q, want it to be the %s", err.Error(), tc.firstLeg)
			}
		})
	}
}
