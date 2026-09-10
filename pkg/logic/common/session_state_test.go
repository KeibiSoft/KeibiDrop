// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"errors"
	"testing"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestSessionState_Texts(t *testing.T) {
	require.Equal(t, "Connected on the local network", connectedText("lan", false, false, false))
	require.Equal(t, "Connected directly, slow link", connectedText("direct", false, true, false))
	require.Equal(t, "Connected via relay, paid lane", connectedText("bridge", true, false, false))
	require.Equal(t, "Connected via relay, free lane", connectedText("bridge", false, false, false))
	require.Equal(t, "Connected via relay, free lane, shared right now", connectedText("bridge", false, false, true))
	require.Equal(t, "Connected via relay, free lane, shared right now", connectedText("bridge", false, true, true), "the lane a person can change comes before the one they can only wait out")
	require.Equal(t, "Waiting for the other side", waitingText("Waiting for peer...", ""))
	require.Equal(t, "Waiting for nas", waitingText("Waiting for peer...", "nas"))
	require.Equal(t, "nas has not pressed Connect yet", waitingText("peer_not_ready", "nas"))
	require.Equal(t, "Peer away", awayLabel(""))
	require.Equal(t, "nas is away", awayLabel("nas"))
}

func TestSessionState_ConnectWaitIsShownThenCleared(t *testing.T) {
	kd := &KeibiDrop{}
	var events []string
	kd.OnEvent = func(e string) { events = append(events, e) }

	require.Equal(t, StateIdle, kd.SessionState().State)
	kd.emitConnectStatus("Waiting for peer...")
	st := kd.SessionState()
	require.Equal(t, StateWaitingForPeer, st.State)
	require.Equal(t, "Waiting for the other side", st.Text)
	require.Equal(t, []string{"connect_status:Waiting for peer..."}, events)

	kd.clearConnectStatus()
	require.Equal(t, StateIdle, kd.SessionState().State)
	require.Equal(t, "Not connected", kd.SessionState().Text)
}

func TestBytesPerSecond(t *testing.T) {
	require.Equal(t, uint64(2_000_000), bytesPerSecond(3_000_000, 1_000_000, time.Second))
	require.Equal(t, uint64(500_000), bytesPerSecond(1_000_000, 0, 2*time.Second))
	require.Equal(t, uint64(0), bytesPerSecond(5, 10, time.Second), "a counter that went backwards is not a rate")
}

// fakeReadStream answers one block and records how many fetches were in flight
// while it did, so the test can see the hook bracket the network wait.
type fakeReadStream struct {
	grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse]
	inFlight *int
	seen     int
	fail     bool
}

func (f *fakeReadStream) Send(_ *bindings.ReadRequest) error { return nil }
func (f *fakeReadStream) Recv() (*bindings.ReadResponse, error) {
	f.seen = *f.inFlight
	if f.fail {
		return nil, errors.New("stream reset")
	}
	return &bindings.ReadResponse{Data: []byte("block")}, nil
}

// A block fetch counts as in flight from Send to Recv, and is released on the
// error path too, so the health monitor's deferral never inherits a stuck count.
func TestRemoteFileStream_FetchHookBracketsTheRead(t *testing.T) {
	inFlight := 0
	hook := func(active bool) {
		if active {
			inFlight++
		} else {
			inFlight--
		}
	}
	ok := &fakeReadStream{inFlight: &inFlight}
	rfs := NewImplRemoteFileStream(ok, 1, "/f")
	rfs.onFetch = hook
	data, err := rfs.ReadAt(context.Background(), 0, 5)
	require.NoError(t, err)
	require.Equal(t, []byte("block"), data)
	require.Equal(t, 1, ok.seen, "the fetch was counted while Recv waited")
	require.Equal(t, 0, inFlight, "released after the read")

	bad := &fakeReadStream{inFlight: &inFlight, fail: true}
	rfs = NewImplRemoteFileStream(bad, 1, "/f")
	rfs.onFetch = hook
	_, err = rfs.ReadAt(context.Background(), 0, 5)
	require.Error(t, err)
	require.Equal(t, 0, inFlight, "released on the error path")
}

// The provider hands its hook to every stream it opens.
func TestStreamProvider_FetchHookReachesStreams(t *testing.T) {
	calls := &[]string{}
	sp := NewImplStreamProvider(fakeRouteCli{name: "tcp", calls: calls}).WithFetchHook(func(bool) {})
	s, err := sp.OpenRemoteFile(context.Background(), 1, "/f")
	require.NoError(t, err)
	require.NotNil(t, s.(*ImplRemoteFileStream).onFetch)
}

// A fetch in flight defers the disconnect verdict like a pull does.
func TestNoteFetch_CountsAsActiveTransfer(t *testing.T) {
	kd := &KeibiDrop{}
	require.False(t, kd.hasActiveTransfers())
	kd.noteFetch(true)
	require.True(t, kd.hasActiveTransfers())
	kd.noteFetch(false)
	require.False(t, kd.hasActiveTransfers())
}
