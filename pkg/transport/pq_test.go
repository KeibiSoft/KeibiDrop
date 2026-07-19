package transport

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestPQCHandshakeOverQUIC runs the hybrid ML-KEM-1024 + X25519 handshake over a
// QUIC stream, then sends ChaCha20-Poly1305-encrypted data both ways. Proves the
// post-quantum security layer composes with QUIC (both handshake and bulk).
func TestPQCHandshakeOverQUIC(t *testing.T) {
	p := newConnPair(t)
	defer p.close()

	type res struct {
		c   net.Conn
		err error
	}
	srv := make(chan res, 1)
	go func() {
		// server side materializes after the client's first handshake write
		sc := p.awaitServer(t)
		secured, err := Secure(context.Background(), sc, RoleServer)
		srv <- res{secured, err}
	}()

	clientSecured, err := Secure(context.Background(), p.client, RoleClient)
	if err != nil {
		t.Fatalf("client Secure: %v", err)
	}
	sr := <-srv
	if sr.err != nil {
		t.Fatalf("server Secure: %v", sr.err)
	}
	serverSecured := sr.c

	// client -> server, payload larger than one read buffer (exercises leftover)
	msg := bytes.Repeat([]byte("pq"), 8192) // 16 KiB
	werr := make(chan error, 1)
	go func() { _, e := clientSecured.Write(msg); werr <- e }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(serverSecured, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("client->server ciphertext did not round-trip")
	}
	if err := <-werr; err != nil {
		t.Fatalf("client write: %v", err)
	}

	// server -> client
	reply := []byte("pq-ack-from-server")
	if _, err := serverSecured.Write(reply); err != nil {
		t.Fatalf("server write: %v", err)
	}
	rb := make([]byte, len(reply))
	if _, err := io.ReadFull(clientSecured, rb); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(rb, reply) {
		t.Fatal("server->client ciphertext did not round-trip")
	}
}

// TestGRPCOverSecureQUIC is the full Phase-2 stack: gRPC (unary + server-stream)
// over SecureConn (post-quantum) over QUIC. Bytes are verified end to end.
func TestGRPCOverSecureQUIC(t *testing.T) {
	srv, addr, err := startQUICServerSecure("127.0.0.1:0", benchService{})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()

	cc, err := dialQUICSecure(addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()
	client := pb.NewBenchServiceClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// unary over PQC-secured QUIC
	reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: []byte("pq-grpc")})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if string(reply.Payload) != "pq-grpc" {
		t.Fatalf("echo mismatch: %q", reply.Payload)
	}

	// server-stream over PQC-secured QUIC
	const total = 4 << 20
	stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: total, ChunkSize: 64 << 10})
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
		if !verifyChunk(chunk.Data) {
			t.Fatalf("corrupt chunk at offset %d", chunk.Offset)
		}
		got += uint64(len(chunk.Data))
	}
	if got != total {
		t.Fatalf("byte count: got %d want %d", got, total)
	}
}
