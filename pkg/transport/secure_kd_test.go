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

// TestKDHandshakeOverQUIC runs KeibiDrop's post-quantum handshake (persistent X25519 +
// ML-KEM-1024 identities, mutual fingerprint verification) over a QUIC stream, then
// sends AEAD data both ways.
func TestKDHandshakeOverQUIC(t *testing.T) {
	testkit.Run(t, func() error {
		serverID := testkit.Must(NewIdentity())
		clientID := testkit.Must(NewIdentity())

		p := newConnPair(t)
		defer p.close()

		var serverSecured net.Conn
		joinServer := testkit.Go(func() error {
			sc := p.awaitServer(t)
			secured, err := SecureKD(sc, serverID, clientID.Fingerprint(), RoleServer)
			serverSecured = secured
			return err
		})

		clientSecured := testkit.Must(SecureKD(p.client, clientID, serverID.Fingerprint(), RoleClient))
		if err := joinServer(); err != nil {
			return fmt.Errorf("server SecureKD: %w", err)
		}

		// client -> server, larger than one read buffer (exercises leftover buffering)
		msg := bytes.Repeat([]byte("kd"), 8192) // 16 KiB
		writeErr := testkit.Go(func() error {
			_, e := clientSecured.Write(msg)
			return e
		})
		got := make([]byte, len(msg))
		testkit.Must(io.ReadFull(serverSecured, got))
		if err := fp.BytesEqual("client->server round trip", got, msg); err != nil {
			return err
		}
		if err := writeErr(); err != nil {
			return fmt.Errorf("client write: %w", err)
		}

		// server -> client (proves the second, independently-keyed direction)
		reply := []byte("kd-ack-from-server")
		testkit.Must(serverSecured.Write(reply))
		rb := make([]byte, len(reply))
		testkit.Must(io.ReadFull(clientSecured, rb))
		return fp.BytesEqual("server->client round trip", rb, reply)
	})
}

// TestKDFingerprintMismatch checks authentication gates the connection: a server
// expecting a different client fingerprint rejects the handshake.
func TestKDFingerprintMismatch(t *testing.T) {
	testkit.Run(t, func() error {
		serverID, _ := NewIdentity()
		clientID, _ := NewIdentity()
		wrong, _ := NewIdentity() // the client the server *expects*, not the one that connects

		p := newConnPair(t)
		defer p.close()

		joinServer := testkit.Go(func() error {
			sc := p.awaitServer(t)
			_, err := SecureKD(sc, serverID, wrong.Fingerprint(), RoleServer)
			if err != nil {
				_ = sc.Close() // unblock the client's read, mirroring the real listener
			}
			return err
		})

		_, cerr := SecureKD(p.client, clientID, serverID.Fingerprint(), RoleClient)
		_ = cerr // the client also fails once the server drops the conn; the server-side reject is the assertion
		return fp.WantErr("server rejects a client whose fingerprint does not match the expected one", joinServer())
	})
}

// TestGRPCOverKDSecureQUIC is the full stack: gRPC (unary + server-stream) over
// KeibiDrop's PQC handshake over QUIC, with fingerprint authentication on both ends.
func TestGRPCOverKDSecureQUIC(t *testing.T) {
	testkit.Run(t, func() error {
		serverID, _ := NewIdentity()
		clientID, _ := NewIdentity()

		srv, addr := testkit.Must2(ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint()))
		defer srv.Stop()

		cc := testkit.Must(DialGRPCKD(addr.String(), clientID, serverID.Fingerprint()))
		defer cc.Close()
		client := pb.NewBenchServiceClient(cc)

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		reply := testkit.Must(client.Echo(ctx, &pb.EchoRequest{Payload: []byte("kd-pq-grpc")}))
		if err := fp.Equal("echo", string(reply.Payload), "kd-pq-grpc"); err != nil {
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
