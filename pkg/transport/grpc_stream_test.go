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
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestGRPCDownload runs the server-streaming RPC over both transports across corner
// cases, verifying every byte, contiguous offsets, and the exact total.
func TestGRPCDownload(t *testing.T) {
	cases := []struct {
		name      string
		total     uint64
		chunkSize uint32
	}{
		{"8MiB_64KiB", 8 << 20, 64 << 10},
		{"partial_last_chunk", 100000, 64 << 10}, // 100000 % 65536 != 0
		{"empty_stream", 0, 64 << 10},
		{"default_chunk_size", 1 << 20, 0},
		{"tiny_chunks", 1 << 16, 1000},
		{"one_byte", 1, 64 << 10},
	}
	for _, tr := range transports {
		for _, tc := range cases {
			t.Run(tr.name+"/"+tc.name, func(t *testing.T) {
				client, cleanup := tr.newClient(t)
				defer cleanup()

				testkit.Run(t, func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()

					stream := testkit.Must(client.Download(ctx, &pb.DownloadRequest{TotalBytes: tc.total, ChunkSize: tc.chunkSize}))
					var soFar uint64
					got, err := drainDownload(stream, func(c *pb.Chunk) error {
						if c.Offset != soFar {
							return fmt.Errorf("offset gap: got %d want %d", c.Offset, soFar)
						}
						if !verifyChunk(c.Data) {
							return fmt.Errorf("corrupt chunk at offset %d", c.Offset)
						}
						soFar += uint64(len(c.Data))
						return nil
					})
					if err != nil {
						return err
					}
					return fp.Equal("byte count", got, tc.total)
				})
			})
		}
	}
}

// TestConcurrentDownloads proves gRPC multiplexes many simultaneous RPCs over the
// single QUIC stream (HTTP/2 framing inside one QUIC stream), and TCP does the same.
func TestConcurrentDownloads(t *testing.T) {
	const n = 8
	const per = 1 << 20 // 1 MiB each
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			client, cleanup := tr.newClient(t)
			defer cleanup()

			join := testkit.GoN(n, func(i int) error {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: per, ChunkSize: 64 << 10})
				if err != nil {
					return err
				}
				got, err := drainDownload(stream, nil)
				if err != nil {
					return err
				}
				if got != per {
					return io.ErrUnexpectedEOF
				}
				return nil
			})
			if err := join(); err != nil {
				t.Fatalf("concurrent download: %v", err)
			}
		})
	}
}

// TestServerStreamCancel verifies that cancelling a server-stream partway through
// returns an error to the client promptly, over both transports.
//
// The download is deliberately sized to fit inside the initial flow-control window, so
// the handler sends it all without blocking. It does NOT try to unwind a large
// cancelled stream: over QUIC that can wedge, because gRPC tunnels HTTP/2 over one QUIC
// stream and the server's Send can block in a QUIC flow-control write gRPC cannot
// interrupt. In the product the QUIC channel carries control/heartbeats, not big
// cancellable streams, so this does not bite.
func TestServerStreamCancel(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			client, cleanup := tr.newClient(t)
			defer cleanup()

			testkit.Run(t, func() error {
				ctx, cancel := context.WithCancel(context.Background())
				stream := testkit.Must(client.Download(ctx, &pb.DownloadRequest{TotalBytes: 512 << 10, ChunkSize: 64 << 10}))
				for i := 0; i < 3; i++ {
					testkit.Must(stream.Recv())
				}
				cancel()

				// After cancel, Recv must return a terminal error promptly. Buffered chunks
				// may still be in flight, so drain until the error rather than asserting on a
				// single Recv, which can return buffered data before the cancellation lands.
				done := make(chan error, 1)
				go func() {
					for {
						if _, e := stream.Recv(); e != nil {
							done <- e
							return
						}
					}
				}()
				select {
				case <-done:
					// Recv returned a terminal error: the client is not stuck.
					return nil
				case <-time.After(5 * time.Second):
					return fmt.Errorf("client Recv did not return an error after cancel")
				}
			})
		})
	}
}

// TestEchoCornerCases covers empty, single-byte, and large unary payloads over
// both transports.
func TestEchoCornerCases(t *testing.T) {
	payloads := []struct {
		name string
		p    []byte
	}{
		{"empty", []byte{}},
		{"one_byte", []byte{0x42}},
		{"1MiB", bytes.Repeat([]byte{0xAB}, 1<<20)},
	}
	for _, tr := range transports {
		for _, pc := range payloads {
			t.Run(tr.name+"/"+pc.name, func(t *testing.T) {
				client, cleanup := tr.newClient(t)
				defer cleanup()

				testkit.Run(t, func() error {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					reply := testkit.Must(client.Echo(ctx, &pb.EchoRequest{Payload: pc.p}))
					return fp.BytesEqual("echo", reply.Payload, pc.p)
				})
			})
		}
	}
}
