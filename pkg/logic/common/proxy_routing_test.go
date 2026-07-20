// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.

package common

import (
	"context"
	"errors"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// fakeRouteCli records which client each RPC landed on and optionally fails, so the
// routing (preferFast / preferBulk) is testable without any network. Unused methods
// come from the embedded nil interface (calling them would panic — the point).
type fakeRouteCli struct {
	bindings.KeibiServiceClient
	name  string
	fail  bool
	calls *[]string
}

func (f fakeRouteCli) note(rpc string) error {
	*f.calls = append(*f.calls, f.name+"."+rpc)
	if f.fail {
		return errors.New(f.name + " is down")
	}
	return nil
}

func (f fakeRouteCli) Read(_ context.Context, _ ...grpc.CallOption) (grpc.BidiStreamingClient[bindings.ReadRequest, bindings.ReadResponse], error) {
	return nil, f.note("Read")
}

func (f fakeRouteCli) StreamFile(_ context.Context, _ *bindings.StreamFileRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[bindings.StreamFileResponse], error) {
	return nil, f.note("StreamFile")
}

// TestProviderRouting pins the channel-routing contract of the dual stream provider:
//   - on-demand reads prefer QUIC (fast), falling back to TCP (bulk) when QUIC is dead
//   - bulk StreamFile prefers TCP, falling back to QUIC when TCP is dead — files keep
//     flowing (slower) until the reconnect brings TCP back
//   - a single-client provider behaves exactly as before
func TestProviderRouting(t *testing.T) {
	ctx := context.Background()

	setup := func(bulkDown, fastDown bool) (*ImplFileStreamProvider, *[]string) {
		calls := &[]string{}
		return NewImplStreamProviderDual(
			fakeRouteCli{name: "tcp", fail: bulkDown, calls: calls},
			fakeRouteCli{name: "quic", fail: fastDown, calls: calls},
		), calls
	}

	t.Run("both alive: reads on QUIC, bulk on TCP", func(t *testing.T) {
		sp, calls := setup(false, false)
		_, err := sp.OpenRemoteFile(ctx, 1, "/f")
		require.NoError(t, err)
		_, err = sp.StreamFile(ctx, "/f", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"quic.Read", "tcp.StreamFile"}, *calls)
	})

	t.Run("QUIC dead: reads fall back to TCP", func(t *testing.T) {
		sp, calls := setup(false, true)
		_, err := sp.OpenRemoteFile(ctx, 1, "/f")
		require.NoError(t, err)
		require.Equal(t, []string{"quic.Read", "tcp.Read"}, *calls)
	})

	t.Run("TCP dead: bulk continues over QUIC", func(t *testing.T) {
		sp, calls := setup(true, false)
		_, err := sp.StreamFile(ctx, "/f", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"tcp.StreamFile", "quic.StreamFile"}, *calls)
	})

	t.Run("both dead: the error surfaces (relay reconnect takes over)", func(t *testing.T) {
		sp, _ := setup(true, true)
		_, err := sp.StreamFile(ctx, "/f", 0)
		require.Error(t, err)
	})

	t.Run("single-client provider unchanged", func(t *testing.T) {
		calls := &[]string{}
		sp := NewImplStreamProvider(fakeRouteCli{name: "tcp", calls: calls})
		_, err := sp.OpenRemoteFile(ctx, 1, "/f")
		require.NoError(t, err)
		_, err = sp.StreamFile(ctx, "/f", 0)
		require.NoError(t, err)
		require.Equal(t, []string{"tcp.Read", "tcp.StreamFile"}, *calls)
	})
}
