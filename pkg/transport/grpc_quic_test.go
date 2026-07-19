package transport

import (
	"bytes"
	"context"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestGRPCUnaryOverQUIC is the core proof of the whole track: a real gRPC unary
// RPC completing over a QUIC stream, through the quicConn adapter, the
// quicListener, and grpc.NewClient with the passthrough dialer.
func TestGRPCUnaryOverQUIC(t *testing.T) {
	srv, addr, err := startQUICServer("127.0.0.1:0", benchService{})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()

	cc, err := dialQUIC(addr.String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	client := pb.NewBenchServiceClient(cc)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	payload := []byte("hello over quic")
	reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: payload})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if !bytes.Equal(reply.Payload, payload) {
		t.Fatalf("echo mismatch: got %q want %q", reply.Payload, payload)
	}
}
