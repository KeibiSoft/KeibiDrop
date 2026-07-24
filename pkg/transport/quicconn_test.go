package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// connPair is a live QUIC connection wrapped as net.Conn on both ends. The server side
// arrives only after the client first writes, because QUIC opens streams lazily
// (AcceptStream returns on the first STREAM frame), as gRPC does with its HTTP/2 preface.
type connPair struct {
	client   net.Conn
	ln       *quic.Listener
	serverCh chan net.Conn
	server   net.Conn
}

func newConnPair(t *testing.T) *connPair {
	t.Helper()
	ln, err := quic.ListenAddr("127.0.0.1:0", serverTLSConfig(), nil)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	serverCh := make(chan net.Conn, 1)
	go func() {
		sconn, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		sstream, err := sconn.AcceptStream(context.Background())
		if err != nil {
			return
		}
		serverCh <- newQUICConn(sconn, sstream)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cconn, err := quic.DialAddr(ctx, ln.Addr().String(), clientTLSConfig(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	cstream, err := cconn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	return &connPair{client: newQUICConn(cconn, cstream), ln: ln, serverCh: serverCh}
}

// awaitServer returns the server-side net.Conn (blocks until the client's first write).
func (p *connPair) awaitServer(t *testing.T) net.Conn {
	t.Helper()
	select {
	case p.server = <-p.serverCh:
		return p.server
	case <-time.After(5 * time.Second):
		t.Fatal("server side never materialized (did the client write?)")
		return nil
	}
}

func (p *connPair) close() {
	if p.server != nil {
		_ = p.server.Close()
	}
	_ = p.client.Close()
	_ = p.ln.Close()
}

// TestQUICConnRoundTrip checks the adapter carries bytes both directions with a payload
// larger than one read buffer, exercising partial reads.
func TestQUICConnRoundTrip(t *testing.T) {
	p := newConnPair(t)
	defer p.close()

	msg := bytes.Repeat([]byte("kd"), 8192) // 16 KiB
	writeErr := make(chan error, 1)
	go func() { _, err := p.client.Write(msg); writeErr <- err }()

	server := p.awaitServer(t)
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(server, got); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatal("client->server payload mismatch")
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("client write: %v", err)
	}

	// Reverse direction.
	reply := []byte("ack-from-server")
	if _, err := server.Write(reply); err != nil {
		t.Fatalf("server write: %v", err)
	}
	rbuf := make([]byte, len(reply))
	if _, err := io.ReadFull(p.client, rbuf); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if !bytes.Equal(rbuf, reply) {
		t.Fatal("server->client reply mismatch")
	}
}

// TestQUICConnReadDeadline proves SetReadDeadline is honored (gRPC relies on
// deadline semantics of the underlying net.Conn).
func TestQUICConnReadDeadline(t *testing.T) {
	p := newConnPair(t)
	defer p.close()

	// Prime the stream so the server side exists, then drain the primer byte.
	go func() { _, _ = p.client.Write([]byte("x")) }()
	server := p.awaitServer(t)
	_, _ = io.ReadFull(server, make([]byte, 1))

	// No further data is coming; a read with a short deadline must time out.
	if err := p.client.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	_, err := p.client.Read(make([]byte, 8))
	if err == nil {
		t.Fatal("expected a deadline error, got nil")
	}
	var ne net.Error
	isTimeout := errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
	if !isTimeout {
		t.Fatalf("expected a timeout error, got %T: %v", err, err)
	}
}

// TestQUICConnCloseNoLeak runs many connect/write/close cycles and asserts the goroutine
// count settles near baseline, i.e. Close does not leak a reader goroutine per conn.
func TestQUICConnCloseNoLeak(t *testing.T) {
	// Warm up so quic-go's global goroutines are running before taking the baseline.
	warm := newConnPair(t)
	go func() { _, _ = warm.client.Write([]byte("x")) }()
	_, _ = io.ReadFull(warm.awaitServer(t), make([]byte, 1))
	warm.close()
	settle(200 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	for i := 0; i < 25; i++ {
		p := newConnPair(t)
		go func() { _, _ = p.client.Write([]byte("hello")) }()
		_, _ = io.ReadFull(p.awaitServer(t), make([]byte, 5))
		p.close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n <= baseline+3 { // small tolerance for scheduler/GC transients
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not settle after 25 cycles: baseline=%d now=%d (possible per-conn leak)", baseline, n)
		}
		settle(25 * time.Millisecond)
	}
}

func settle(d time.Duration) {
	timer := time.NewTimer(d)
	<-timer.C
}
