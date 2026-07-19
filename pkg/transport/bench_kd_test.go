package transport

import (
	"context"
	"io"
	"net"
	"testing"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// BenchmarkKDHandshake isolates the per-connection cost of KeibiDrop's post-quantum
// handshake: 2x ML-KEM-1024 encapsulate/decapsulate + 2x X25519 + HKDF-SHA512 + JSON
// framing, over an in-memory pipe so QUIC's own dial cost is excluded. Identity keys
// are generated once, outside the loop, so this is the handshake, not keygen.
func BenchmarkKDHandshake(b *testing.B) {
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	sfp, cfp := serverID.Fingerprint(), clientID.Fingerprint()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, s := net.Pipe()
		done := make(chan error, 1)
		go func() { _, e := SecureKD(s, serverID, cfp, RoleServer); done <- e }()
		if _, err := SecureKD(c, clientID, sfp, RoleClient); err != nil {
			b.Fatalf("client: %v", err)
		}
		if err := <-done; err != nil {
			b.Fatalf("server: %v", err)
		}
		_ = c.Close()
		_ = s.Close()
	}
}

// startKDClient mirrors transport.newClient but over KeibiDrop's PQC handshake, so
// BenchmarkKDDownload compares throughput against BenchmarkDownload/quic. The delta
// is the double-encryption tax: our SecureConn AEAD on top of QUIC's own packet
// protection (which, on Go 1.24+, is itself already a post-quantum X25519MLKEM768).
func startKDClient(b *testing.B) (pb.BenchServiceClient, func()) {
	b.Helper()
	serverID, _ := NewIdentity()
	clientID, _ := NewIdentity()
	srv, addr, err := ServeGRPCKD("127.0.0.1:0", benchService{}, serverID, clientID.Fingerprint())
	if err != nil {
		b.Fatalf("start: %v", err)
	}
	cc, err := DialGRPCKD(addr.String(), clientID, serverID.Fingerprint())
	if err != nil {
		srv.Stop()
		b.Fatalf("dial: %v", err)
	}
	return pb.NewBenchServiceClient(cc), func() { _ = cc.Close(); srv.Stop() }
}

// BenchmarkKDDownload measures streaming throughput over gRPC over KeibiDrop's PQC
// handshake over QUIC. Read against BenchmarkDownload/quic for the tax.
func BenchmarkKDDownload(b *testing.B) {
	const total = 64 << 20 // 64 MiB per op
	client, cleanup := startKDClient(b)
	defer cleanup()
	ctx := context.Background()

	b.SetBytes(total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: total, ChunkSize: 64 << 10})
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		var got uint64
		for {
			chunk, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatalf("recv: %v", err)
			}
			got += uint64(len(chunk.Data))
		}
		if got != total {
			b.Fatalf("short read: %d/%d", got, total)
		}
	}
}
