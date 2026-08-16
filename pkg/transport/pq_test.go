//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestPQCHandshakeOverQUIC runs the hybrid ML-KEM-1024 + X25519 handshake over a QUIC
// stream, then sends ChaCha20-Poly1305 data both ways.
func TestPQCHandshakeOverQUIC(t *testing.T) {
	testkit.Run(t, func() error {
		p := newConnPair(t)
		defer p.close()

		var serverSecured net.Conn
		joinServer := testkit.Go(func() error {
			// server side materializes after the client's first handshake write
			sc := p.awaitServer(t)
			secured, err := Secure(context.Background(), sc, RoleServer)
			serverSecured = secured
			return err
		})

		clientSecured := testkit.Must(Secure(context.Background(), p.client, RoleClient))
		if err := joinServer(); err != nil {
			return fmt.Errorf("server Secure: %w", err)
		}

		// client -> server, payload larger than one read buffer (exercises leftover)
		msg := bytes.Repeat([]byte("pq"), 8192) // 16 KiB
		writeErr := testkit.Go(func() error {
			_, e := clientSecured.Write(msg)
			return e
		})
		got := make([]byte, len(msg))
		testkit.Must(io.ReadFull(serverSecured, got))
		if err := fp.BytesEqual("client->server ciphertext round trip", got, msg); err != nil {
			return err
		}
		if err := writeErr(); err != nil {
			return fmt.Errorf("client write: %w", err)
		}

		// server -> client
		reply := []byte("pq-ack-from-server")
		testkit.Must(serverSecured.Write(reply))
		rb := make([]byte, len(reply))
		testkit.Must(io.ReadFull(clientSecured, rb))
		return fp.BytesEqual("server->client ciphertext round trip", rb, reply)
	})
}

// TestGRPCOverSecureQUIC runs gRPC (unary + server-stream) over SecureConn (post-quantum)
// over QUIC, verifying bytes end to end.
func TestGRPCOverSecureQUIC(t *testing.T) {
	testkit.Run(t, func() error {
		srv, addr := testkit.Must2(startQUICServerSecure("127.0.0.1:0", benchService{}))
		defer srv.Stop()

		cc := testkit.Must(dialQUICSecure(addr.String()))
		defer cc.Close()
		client := pb.NewBenchServiceClient(cc)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		reply := testkit.Must(client.Echo(ctx, &pb.EchoRequest{Payload: []byte("pq-grpc")}))
		if err := fp.Equal("echo", string(reply.Payload), "pq-grpc"); err != nil {
			return err
		}

		const total = 4 << 20
		stream := testkit.Must(client.Download(ctx, &pb.DownloadRequest{TotalBytes: total, ChunkSize: 64 << 10}))
		got, err := drainDownload(stream, func(c *pb.Chunk) error {
			if !verifyChunk(c.Data) {
				return fmt.Errorf("corrupt chunk at offset %d", c.Offset)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return fp.Equal("byte count", got, uint64(total))
	})
}
