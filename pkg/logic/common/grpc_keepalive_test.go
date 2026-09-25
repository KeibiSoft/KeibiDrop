// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// The native gRPC servers ping an idle client: a browser peer sends no client
// pings and a bridge reaps a silent leg. The test counts the HTTP/2 PING
// frames the server writes while the pair is idle.

package common

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
)

// pingCountingConn keeps a copy of everything the server writes.
type pingCountingConn struct {
	net.Conn
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *pingCountingConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.buf.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

// serverPings walks the HTTP/2 frames the server wrote and counts the PINGs
// it initiated: type 0x6 without the ACK flag.
func (c *pingCountingConn) serverPings() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.buf.Bytes()
	n := 0
	for len(b) >= 9 {
		length := int(b[0])<<16 | int(b[1])<<8 | int(b[2])
		if b[3] == 0x6 && b[4]&0x1 == 0 {
			n++
		}
		if len(b) < 9+length {
			break
		}
		b = b[9+length:]
	}
	return n
}

// countingListener wraps every accepted conn so the test can read it back.
type countingListener struct {
	*bufconn.Listener
	mu    sync.Mutex
	conns []*pingCountingConn
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	pc := &pingCountingConn{Conn: c}
	l.mu.Lock()
	l.conns = append(l.conns, pc)
	l.mu.Unlock()
	return pc, nil
}

func (l *countingListener) pings() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, c := range l.conns {
		n += c.serverPings()
	}
	return n
}

// serverPingsWhileIdle brings a client up against a server built from
// kdServerOptions, leaves the pair idle, and returns the pings the server sent.
func serverPingsWhileIdle(t *testing.T, idle time.Duration) (int, error) {
	t.Helper()
	ln := &countingListener{Listener: bufconn.Listen(1 << 20)}
	srv := grpc.NewServer(kdServerOptions()...)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)

	cc, err := grpc.NewClient("passthrough:///keepalive",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return ln.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return 0, err
	}
	t.Cleanup(func() { _ = cc.Close() })
	cc.Connect()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for s := cc.GetState(); s != connectivity.Ready; s = cc.GetState() {
		if !cc.WaitForStateChange(ctx, s) {
			return 0, fmt.Errorf("the client never became Ready, last state %s", s)
		}
	}
	time.Sleep(idle)
	return ln.pings(), nil
}

// With the keepalive on, an idle client is pinged. With grpc's default (two
// hours, what a 0.4.8 server has) it is not.
func TestServerKeepalive_PingsAnIdleClient(t *testing.T) {
	old := kdServerKeepalive
	t.Cleanup(func() { kdServerKeepalive = old })

	type tc struct {
		name string
		time time.Duration
		want func(pings int) error
	}
	cases := []tc{
		{"keepalive on", 200 * time.Millisecond, func(n int) error {
			return fp.True("the server pings an idle client", n >= 1)
		}},
		{"grpc default", 0, func(n int) error {
			return fp.Equal("pings inside the window without the keepalive", n, 0)
		}},
	}
	testkit.RunTable(t, cases, func(c tc) string { return c.name }, func(t *testing.T, c tc) error {
		kdServerKeepalive.Time = c.time
		n, err := serverPingsWhileIdle(t, 1500*time.Millisecond)
		if err != nil {
			return err
		}
		return c.want(n)
	})
}

// Two intervals fit the bridge's 120 s idle reap (kdwsbridge), and a client
// that pings on its own is accepted.
func TestServerKeepalive_FitsTheBridgeWindow(t *testing.T) {
	testkit.Run(t, func() error {
		return fp.All(
			fp.True("keepalive is on", kdServerKeepalive.Time > 0),
			fp.True("two intervals fit the 120 s bridge reap", kdServerKeepalive.Time*2 <= 120*time.Second),
			fp.True("timeout leaves room for a slow ack", kdServerKeepalive.Timeout > kdServerKeepalive.Time/2),
			fp.True("a client may ping with no stream open", kdServerKeepalivePolicy.PermitWithoutStream),
			fp.True("a client pinging at our own interval is admitted", kdServerKeepalivePolicy.MinTime <= kdServerKeepalive.Time),
		)
	})
}
