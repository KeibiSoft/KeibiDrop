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

// TestKDHandshakeOverQUIC runs KeibiDrop's real post-quantum handshake (persistent
// X25519 + ML-KEM-1024 identities, mutual fingerprint verification, pkg/crypto
// unchanged) over a QUIC stream, then sends AEAD data both ways. Proves the
// product's actual security layer composes with QUIC.
func TestKDHandshakeOverQUIC(t *testing.T) {
	serverID, err := NewIdentity()
	if err != nil {
		t.Fatalf("server identity: %v", err)
	}
	clientID, err := NewIdentity()
	if err != nil {
		t.Fatalf("client identity: %v", err)
	}

	p := newConnPair(t)
	defer p.close()

	type res struct {
		c   net.Conn
		err error
	}
	srv := make(chan res, 1)
	go func() {
		sc := p.awaitServer(t)
		secured, err := SecureKD(sc, serverID, clientID.Fingerprint(), RoleServer)
		srv <- res{secured, err}
	}()

	clientSecured, err := SecureKD(p.client, clientID, serverID.Fingerprint(), RoleClient)
	if err != nil {
		t.Fatalf("client SecureKD: %v", err)
	}
	sr := <-srv
	if sr.err != nil {
		t.Fatalf("server SecureKD: %v", sr.err)
	}
	serverSecured := sr.c

	// client -> server, larger than one read buffer (exercises leftover buffering)
	msg := bytes.Repeat([]byte("kd"), 8192) // 16 KiB
	werr := make(chan error, 1)
	go func() { _, e := clientSecured.Write(msg); werr <- e }()
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(serverSecured, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("client->server did not round-trip")
	}
	if err := <-werr; err != nil {
		t.Fatalf("client write: %v", err)
	}

	// server -> client (proves the second, independently-keyed direction)
	reply := []byte("kd-ack-from-server")
	if _, err := serverSecured.Write(reply); err != nil {
		t.Fatalf("server write: %v", err)
	}
	rb := make([]byte, len(reply))
	if _, err := io.ReadFull(clientSecured, rb); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(rb, reply) {
		t.Fatal("server->client did not round-trip")
	}
}

// TestKDFingerprintMismatch proves the authentication actually gates the connection:
// a server expecting a different client fingerprint rejects the handshake. This is
// the security property, not just "it runs".
func TestKDFingerprintMismatch(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	wrong, _ := NewIdentity() // the client the server *expects*, not the one that connects

	p := newConnPair(t)
	defer p.close()

	srvErr := make(chan error, 1)
	go func() {
		sc := p.awaitServer(t)
		_, err := SecureKD(sc, serverID, wrong.Fingerprint(), RoleServer)
		if err != nil {
			_ = sc.Close() // unblock the client's read, mirroring the real listener
		}
		srvErr <- err
	}()

	_, cerr := SecureKD(p.client, clientID, serverID.Fingerprint(), RoleClient)
	serr := <-srvErr
	if serr == nil {
		t.Fatal("server accepted a client whose fingerprint did not match the expected one")
	}
	_ = cerr // the client also fails once the server drops the conn; the server-side reject is the assertion
}

// TestGRPCOverKDSecureQUIC is the full stack: gRPC (unary + server-stream) over
// KeibiDrop's PQC handshake over QUIC, with fingerprint authentication on both ends.
func TestGRPCOverKDSecureQUIC(t *testing.T) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()

	srv, addr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()

	cc, err := DialGRPCKD(addr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()
	client := pb.NewBenchServiceClient(cc)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: []byte("kd-pq-grpc")})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if string(reply.Payload) != "kd-pq-grpc" {
		t.Fatalf("echo mismatch: %q", reply.Payload)
	}

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
