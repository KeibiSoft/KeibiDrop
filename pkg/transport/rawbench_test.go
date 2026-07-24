package transport

import (
	"context"
	"io"
	"net"
	"testing"

	"github.com/quic-go/quic-go"
)

// BenchmarkRawTransport measures raw throughput with no gRPC: a single QUIC stream vs a
// single TCP connection. This separates the cost of the QUIC transport from the cost of
// tunnelling gRPC/HTTP-2 over it:
//
//   - raw QUIC ~= gRPC-over-QUIC -> the ceiling is the QUIC transport, not tunnelling.
//   - raw QUIC >> gRPC-over-QUIC -> the single-stream HTTP/2 tunnel is the bottleneck.
func BenchmarkRawTransport(b *testing.B) {
	const total = 32 << 20 // 32 MiB per op

	b.Run("quic", func(b *testing.B) {
		ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSConfig(), quicConfig())
		if err != nil {
			b.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept(context.Background())
			if err != nil {
				return
			}
			st, err := conn.AcceptStream(context.Background())
			if err != nil {
				return
			}
			rawSource(st, total)
		}()
		conn, err := quic.DialAddr(context.Background(), ln.Addr().String(), clientTLSConfig(), quicConfig())
		if err != nil {
			b.Fatal(err)
		}
		defer conn.CloseWithError(0, "")
		st, err := conn.OpenStreamSync(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		rawSink(b, st, total)
	})

	b.Run("tcp", func(b *testing.B) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		defer ln.Close()
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			rawSource(conn, total)
		}()
		conn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		rawSink(b, conn, total)
	})
}

// rawSource loops: read a 1-byte nudge, then write `total` bytes.
func rawSource(rw io.ReadWriter, total int) {
	buf := make([]byte, 256<<10)
	nudge := make([]byte, 1)
	for {
		if _, err := io.ReadFull(rw, nudge); err != nil {
			return
		}
		sent := 0
		for sent < total {
			n := len(buf)
			if total-sent < n {
				n = total - sent
			}
			if _, err := rw.Write(buf[:n]); err != nil {
				return
			}
			sent += n
		}
	}
}

// rawSink drives b.N transfers: nudge, then read exactly `total` bytes.
func rawSink(b *testing.B, rw io.ReadWriter, total int) {
	buf := make([]byte, 256<<10)
	b.SetBytes(int64(total))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := rw.Write([]byte{1}); err != nil {
			b.Fatal(err)
		}
		got := 0
		for got < total {
			n, err := rw.Read(buf)
			got += n
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
