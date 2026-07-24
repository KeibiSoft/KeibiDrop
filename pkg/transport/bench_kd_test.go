package transport

import (
	"context"
	"io"
	"net"
	"testing"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// BenchmarkKDHandshake measures the PQC handshake cost (2x ML-KEM-1024 + 2x X25519 +
// HKDF-SHA512 + JSON framing) over an in-memory pipe. Keys are generated outside the
// loop, so this measures the handshake, not keygen.
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

// startKDClient mirrors newClient but over KeibiDrop's PQC handshake, so
// BenchmarkKDDownload measures the double-encryption tax against BenchmarkDownload/quic
// (SecureConn AEAD on top of QUIC's own packet protection).
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

// BenchmarkKDDownload measures streaming throughput over gRPC over the PQC handshake
// over QUIC. Compare to BenchmarkDownload/quic for the encryption tax.
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
