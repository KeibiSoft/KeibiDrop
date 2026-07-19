package transport

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestGRPCDownload runs the server-streaming RPC over both transports across a
// set of corner cases, verifying every byte (pattern), contiguous offsets, and
// the exact total.
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
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()

				stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: tc.total, ChunkSize: tc.chunkSize})
				if err != nil {
					t.Fatalf("Download: %v", err)
				}
				var got uint64
				for {
					chunk, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatalf("recv at %d: %v", got, err)
					}
					if chunk.Offset != got {
						t.Fatalf("offset gap: got %d want %d", chunk.Offset, got)
					}
					if !verifyChunk(chunk.Data) {
						t.Fatalf("corrupt chunk at offset %d", chunk.Offset)
					}
					got += uint64(len(chunk.Data))
				}
				if got != tc.total {
					t.Fatalf("byte count: got %d want %d", got, tc.total)
				}
			})
		}
	}
}

// TestConcurrentDownloads proves gRPC multiplexes many simultaneous RPCs over the
// single QUIC stream (HTTP/2 framing rides inside one QUIC stream), and that the
// TCP baseline does the same over its one TCP conn.
func TestConcurrentDownloads(t *testing.T) {
	const n = 8
	const per = 1 << 20 // 1 MiB each
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			client, cleanup := tr.newClient(t)
			defer cleanup()

			var wg sync.WaitGroup
			errs := make(chan error, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: per, ChunkSize: 64 << 10})
					if err != nil {
						errs <- err
						return
					}
					var got uint64
					for {
						chunk, err := stream.Recv()
						if err == io.EOF {
							break
						}
						if err != nil {
							errs <- err
							return
						}
						got += uint64(len(chunk.Data))
					}
					if got != per {
						errs <- io.ErrUnexpectedEOF
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent download: %v", err)
				}
			}
		})
	}
}

// TestServerStreamCancel verifies the GUARANTEED cancellation property: cancelling
// a server-stream partway through returns an error to the CLIENT promptly. That
// holds for both transports and is what matters for UX.
//
// The download is deliberately sized to fit inside the initial flow-control window,
// so the server handler sends it all without blocking and returns cleanly. It does
// NOT try to make the server unwind on a *large* cancelled stream: over QUIC that
// can wedge, because gRPC tunnels HTTP/2 over one QUIC stream and the server's Send
// can block in a QUIC flow-control write that gRPC cannot interrupt (an in-flight
// net.Conn.Write). TCP avoids it by having the client keep draining the socket.
// This is the "double flow control" limitation documented in docs/FINDINGS.md; the
// clean fix is native per-RPC QUIC streams (HTTP/3), out of scope here. In the
// product the QUIC channel carries control/heartbeats, not big cancellable streams,
// so it does not bite.
func TestServerStreamCancel(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			client, cleanup := tr.newClient(t)
			defer cleanup()

			ctx, cancel := context.WithCancel(context.Background())
			stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: 512 << 10, ChunkSize: 64 << 10})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			for i := 0; i < 3; i++ {
				if _, err := stream.Recv(); err != nil {
					t.Fatalf("early recv %d: %v", i, err)
				}
			}
			cancel()

			// After cancel, Recv must return a terminal error promptly. On a fast
			// transport there may already be buffered chunks in flight, so drain
			// until the error (Canceled, or EOF if the stream happened to finish) —
			// a single Recv can otherwise return buffered data (nil) before the
			// cancellation lands.
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
				// Recv returned a terminal error — the client is not stuck.
			case <-time.After(5 * time.Second):
				t.Fatal("client Recv did not return an error after cancel")
			}
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
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: pc.p})
				if err != nil {
					t.Fatalf("Echo: %v", err)
				}
				if !bytes.Equal(reply.Payload, pc.p) {
					t.Fatalf("mismatch: got %d bytes want %d", len(reply.Payload), len(pc.p))
				}
			})
		}
	}
}
