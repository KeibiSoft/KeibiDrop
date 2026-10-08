//go:build bench

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2025 KeibiSoft S.R.L.

package transport

import (
	"context"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestGRPCUnaryOverQUIC runs a real gRPC unary RPC over a QUIC stream, through the
// quicConn adapter, the quicListener, and grpc.NewClient with the passthrough dialer.
func TestGRPCUnaryOverQUIC(t *testing.T) {
	testkit.Run(t, func() error {
		srv, addr := testkit.Must2(startQUICServer("127.0.0.1:0", benchService{}))
		defer srv.Stop()

		cc := testkit.Must(dialQUIC(addr.String()))
		defer cc.Close()

		client := pb.NewBenchServiceClient(cc)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		payload := []byte("hello over quic")
		reply := testkit.Must(client.Echo(ctx, &pb.EchoRequest{Payload: payload}))
		return fp.BytesEqual("echo", reply.Payload, payload)
	})
}
