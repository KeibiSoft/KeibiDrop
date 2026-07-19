package transport

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// TestQUICEcho is the smallest possible proof that quic-go v0.60 compiles and
// runs on this machine, using the v0.60 concrete-struct API (*quic.Listener,
// *quic.Conn, *quic.Stream). Server echoes bytes on the first stream the client
// opens; client round-trips a message. No gRPC, no adapter — just the library.
func TestQUICEcho(t *testing.T) {
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSConfig(), nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	serverErr := make(chan error, 1)
	go func() {
		conn, err := ln.Accept(context.Background())
		if err != nil {
			serverErr <- err
			return
		}
		stream, err := conn.AcceptStream(context.Background())
		if err != nil {
			serverErr <- err
			return
		}
		_, err = io.Copy(stream, stream) // echo until the client half-closes
		serverErr <- err
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLSConfig(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseWithError(0, "")

	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}

	msg := []byte("keibidrop-quic-echo")
	if _, err := stream.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: got %q want %q", buf, msg)
	}
}
