// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSetModeAfterReconnect_FollowsTheTransportThatWorked(t *testing.T) {
	cases := []struct {
		name      string
		before    string
		transport string
		local     bool
		want      string
		event     bool
	}{
		{"direct session fell back to the bridge", "direct", "bridge", false, "bridge", true},
		{"bridge session came back direct", "bridge", "direct", false, "direct", true},
		{"lan session came back direct", "lan", "direct", true, "lan", false},
		{"mixed session came back mixed", ModeDirectOut, "mixed", false, ModeDirectOut, false},
		{"mixed session fell back to the bridge", ModeDirectIn, "bridge", false, "bridge", true},
		{"nothing reconnected yet", "direct", "", false, "direct", false},
		{"same transport is not news", "bridge", "bridge", false, "bridge", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kd := &KeibiDrop{logger: quietLogger(), IsLocalMode: tc.local}
			kd.ConnectionMode = tc.before
			var events []string
			kd.OnEvent = func(e string) { events = append(events, e) }
			kd.setModeAfterReconnect(tc.transport)
			require.Equal(t, tc.want, kd.ConnectionMode)
			if tc.event {
				require.Equal(t, []string{"connection_mode:" + tc.want}, events)
			} else {
				require.Empty(t, events)
			}
		})
	}
}

func TestMixedLegs_OnlyTheTwoMixedModes(t *testing.T) {
	kd := &KeibiDrop{}
	for _, m := range []string{"", "lan", "direct", "bridge"} {
		kd.ConnectionMode = m
		require.False(t, kd.mixedLegs(), m)
	}
	for _, m := range []string{ModeDirectOut, ModeDirectIn} {
		kd.ConnectionMode = m
		require.True(t, kd.mixedLegs(), m)
	}
}

// The capability rides the registration; a record from an older peer has no field
// and decodes as false, so the joiner keeps the old behaviour against it.
func TestRegistration_MixedLegsCapabilityRoundTrip(t *testing.T) {
	reg := PeerRegistration{Fingerprint: "fp", Listen: &ConnectionHint{IP: "::1", Port: 26431}, MixedLegs: true}
	b, err := json.Marshal(reg)
	require.NoError(t, err)
	require.Contains(t, string(b), `"mixed_legs":true`)
	var back PeerRegistration
	require.NoError(t, json.Unmarshal(b, &back))
	require.True(t, back.MixedLegs)

	var old PeerRegistration
	require.NoError(t, json.Unmarshal([]byte(`{"fingerprint":"fp","listen":{"ip":"::1","port":26431}}`), &old))
	require.False(t, old.MixedLegs, "an older peer's record decodes as no capability")
	plain, err := json.Marshal(PeerRegistration{Fingerprint: "fp"})
	require.NoError(t, err)
	require.NotContains(t, string(plain), "mixed_legs", "omitted when false, so the blob does not grow for nothing")
}

func TestConnectedText_MixedModes(t *testing.T) {
	require.Equal(t, "Connected directly for reads, relay for the rest", connectedText(ModeDirectOut, false, false, false))
	require.Equal(t, "Connected directly for reads, relay for the rest, paid lane", connectedText(ModeDirectOut, true, false, false))
	require.Equal(t, "Connected, peer reads directly, relay for the rest", connectedText(ModeDirectIn, false, false, false))
	require.Equal(t, "Connected, peer reads directly, relay for the rest, slow link", connectedText(ModeDirectIn, false, true, false))
}
