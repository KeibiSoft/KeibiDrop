//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// countingPacketConn wraps a net.PacketConn and counts datagrams each direction, so a
// test can prove which path QUIC uses. Wrapping also opts out of quic-go's *net.UDPConn
// fast paths, fine for a correctness test.
type countingPacketConn struct {
	net.PacketConn
	reads  atomic.Int64
	writes atomic.Int64
}

func (c *countingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	n, a, err := c.PacketConn.ReadFrom(p)
	if n > 0 {
		c.reads.Add(1)
	}
	return n, a, err
}

func (c *countingPacketConn) WriteTo(p []byte, a net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, a)
	if n > 0 {
		c.writes.Add(1)
	}
	return n, err
}

// TestConnectionMigration runs gRPC unary pings over QUIC, moves the client to a new
// UDP socket mid-flight (AddPath -> Probe -> Switch), and proves via per-socket packet
// counters that traffic moves to the new path in both directions (client sends and
// server responses), i.e. the server migrated its send path too.
//
// Ordering constraints: Switch before Probe fails; wait for ACKs before Switch; the
// client must send after Switch to migrate the peer; LocalAddr updates only after that.
// The original Transport is left open, since quic-go ties the connection's lifetime to it.
func TestConnectionMigration(t *testing.T) {
	srv, addr, err := startQUICServer("127.0.0.1:0", benchService{})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer srv.Stop()
	serverAddr, err := net.ResolveUDPAddr("udp", addr.String())
	if err != nil {
		t.Fatalf("resolve server addr: %v", err)
	}

	// Client dials over an explicit Transport (tr1) on a socket we own + count.
	udp1, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp1: %v", err)
	}
	cudp1 := &countingPacketConn{PacketConn: udp1}
	tr1 := &quic.Transport{Conn: cudp1}
	defer tr1.Close()

	qconnCh := make(chan *quic.Conn, 1)
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		qc, err := tr1.Dial(ctx, serverAddr, clientTLSConfig(), quicConfig())
		if err != nil {
			return nil, err
		}
		st, err := qc.OpenStreamSync(ctx)
		if err != nil {
			_ = qc.CloseWithError(0, "")
			return nil, err
		}
		select {
		case qconnCh <- qc:
		default:
		}
		return newQUICConn(qc, st), nil
	}
	cc, err := grpc.NewClient("passthrough:///migrate",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer cc.Close()
	client := pb.NewBenchServiceClient(cc)

	ping := func(phase string) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: []byte(phase)})
		if err != nil {
			t.Fatalf("ping %s: %v", phase, err)
		}
		if string(reply.Payload) != phase {
			t.Fatalf("ping %s: echo mismatch %q", phase, reply.Payload)
		}
	}

	// Establish the channel on path 1 (the first ping also triggers the dial).
	for i := 0; i < 5; i++ {
		ping("path1")
	}
	var qconn *quic.Conn
	select {
	case qconn = <-qconnCh:
	case <-time.After(5 * time.Second):
		t.Fatal("dialer never produced a quic.Conn")
	}
	addrBefore := qconn.LocalAddr()
	if cudp1.writes.Load() == 0 {
		t.Fatal("path 1 carried no traffic — test setup wrong")
	}

	// Add + validate a second path on a new counted socket.
	udp2, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("udp2: %v", err)
	}
	cudp2 := &countingPacketConn{PacketConn: udp2}
	tr2 := &quic.Transport{Conn: cudp2}
	defer tr2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path, err := qconn.AddPath(tr2)
	if err != nil {
		t.Fatalf("AddPath: %v", err)
	}
	if err := path.Switch(); !errors.Is(err, quic.ErrPathNotValidated) {
		t.Fatalf("Switch before Probe: got %v, want ErrPathNotValidated", err)
	}
	if err := path.Probe(ctx); err != nil {
		t.Fatalf("Probe (path validation): %v", err)
	}
	time.Sleep(30 * time.Millisecond) // let PATH_CHALLENGE/RESPONSE ACKs settle
	if err := path.Switch(); err != nil {
		t.Fatalf("Switch: %v", err)
	}

	// Baselines at the moment of switch.
	w1 := cudp1.writes.Load()
	r2, w2 := cudp2.reads.Load(), cudp2.writes.Load()

	// Keep pinging so the client sends from the new address; the server validates it and
	// migrates its own send path to match.
	for i := 0; i < 25; i++ {
		ping("path2")
	}

	addrAfter := qconn.LocalAddr()
	t.Logf("migration: local addr %v -> %v", addrBefore, addrAfter)
	t.Logf("path1 writes +%d | path2 writes +%d, reads +%d",
		cudp1.writes.Load()-w1, cudp2.writes.Load()-w2, cudp2.reads.Load()-r2)

	// 1) The connection's local address changed to the new socket.
	if addrBefore.String() == addrAfter.String() {
		t.Fatalf("LocalAddr unchanged (%v): switch did not take effect", addrAfter)
	}
	if addrAfter.String() != udp2.LocalAddr().String() {
		t.Fatalf("LocalAddr %v != new socket %v", addrAfter, udp2.LocalAddr())
	}
	// 2) The client now SENDS on path 2 (its writes went there, not path 1).
	if grew := cudp2.writes.Load() - w2; grew < 20 {
		t.Fatalf("path 2 client sends did not grow (+%d); switch ineffective", grew)
	}
	if grew := cudp1.writes.Load() - w1; grew > 5 {
		t.Fatalf("path 1 still sending after switch (+%d); did not migrate", grew)
	}
	// 3) The server RESPONDS on path 2 (client receives there), proving the peer
	//    migrated its send path too.
	if grew := cudp2.reads.Load() - r2; grew < 20 {
		t.Fatalf("path 2 received no server responses (+%d); server did not migrate", grew)
	}
}
