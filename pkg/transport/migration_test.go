//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
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
	testkit.Run(t, func() error {
		srv, addr := testkit.Must2(startQUICServer("127.0.0.1:0", benchService{}))
		defer srv.Stop()
		serverAddr := testkit.Must(net.ResolveUDPAddr("udp", addr.String()))

		// Client dials over an explicit Transport (tr1) on a socket we own + count.
		udp1 := testkit.Must(net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}))
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
		cc := testkit.Must(grpc.NewClient("passthrough:///migrate",
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(dialer)))
		defer cc.Close()
		client := pb.NewBenchServiceClient(cc)

		ping := func(phase string) error {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			reply, err := client.Echo(ctx, &pb.EchoRequest{Payload: []byte(phase)})
			if err != nil {
				return fmt.Errorf("ping %s: %w", phase, err)
			}
			return fp.Equal(fmt.Sprintf("ping %s echo", phase), string(reply.Payload), phase)
		}

		// Establish the channel on path 1 (the first ping also triggers the dial).
		for i := 0; i < 5; i++ {
			if err := ping("path1"); err != nil {
				return err
			}
		}
		var qconn *quic.Conn
		select {
		case qconn = <-qconnCh:
		case <-time.After(5 * time.Second):
			return fmt.Errorf("dialer never produced a quic.Conn")
		}
		addrBefore := qconn.LocalAddr()
		if cudp1.writes.Load() == 0 {
			return fmt.Errorf("path 1 carried no traffic — test setup wrong")
		}

		// Add + validate a second path on a new counted socket.
		udp2 := testkit.Must(net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}))
		cudp2 := &countingPacketConn{PacketConn: udp2}
		tr2 := &quic.Transport{Conn: cudp2}
		defer tr2.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		path := testkit.Must(qconn.AddPath(tr2))
		if err := fp.ErrIs("Switch before Probe", path.Switch(), quic.ErrPathNotValidated); err != nil {
			return err
		}
		if err := path.Probe(ctx); err != nil {
			return fmt.Errorf("probe (path validation): %w", err)
		}
		time.Sleep(30 * time.Millisecond) // let PATH_CHALLENGE/RESPONSE ACKs settle
		if err := path.Switch(); err != nil {
			return fmt.Errorf("switch: %w", err)
		}

		// Baselines at the moment of switch.
		w1 := cudp1.writes.Load()
		r2, w2 := cudp2.reads.Load(), cudp2.writes.Load()

		// Keep pinging so the client sends from the new address; the server validates it and
		// migrates its own send path to match.
		for i := 0; i < 25; i++ {
			if err := ping("path2"); err != nil {
				return err
			}
		}

		addrAfter := qconn.LocalAddr()
		t.Logf("migration: local addr %v -> %v", addrBefore, addrAfter)
		t.Logf("path1 writes +%d | path2 writes +%d, reads +%d",
			cudp1.writes.Load()-w1, cudp2.writes.Load()-w2, cudp2.reads.Load()-r2)

		return fp.All(
			// The connection's local address changed to the new socket.
			fp.NotEqual("local address after switch", addrAfter.String(), addrBefore.String()),
			fp.Equal("local address after switch", addrAfter.String(), udp2.LocalAddr().String()),
			// The client now sends on path 2 (its writes went there, not path 1).
			fp.True("path 2 client sends grew by 20+ after switch", cudp2.writes.Load()-w2 >= 20),
			fp.True("path 1 sends grew by 5 or less after switch", cudp1.writes.Load()-w1 <= 5),
			// The server responds on path 2 (client receives there), proving the peer
			// migrated its send path too.
			fp.True("path 2 received 20+ server responses after switch", cudp2.reads.Load()-r2 >= 20),
		)
	})
}
