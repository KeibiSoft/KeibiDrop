//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: tcpTransport/TCP() plus the ServeGRPC/DialGRPC gRPC wiring used only by
// ABOUTME: the transport package's own bench harness and benchmarks, not by KeibiDrop.

package transport

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TCP returns a plain-TCP transport.
func TCP() Transport { return tcpTransport{} }

type tcpTransport struct{}

func (tcpTransport) Dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (tcpTransport) Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// ServeGRPC serves svc over the transport at addr ("127.0.0.1:0" for an ephemeral
// port). If secure, each accepted conn gets the PQC handshake before gRPC. Returns
// the grpc server (call Stop) and the bound address.
func ServeGRPC(tr Transport, addr string, svc pb.BenchServiceServer, secure bool, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	ln, err := tr.Listen(addr)
	if err != nil {
		return nil, nil, err
	}
	if secure {
		ln = secureListener{ln}
	}
	s := grpc.NewServer(opts...)
	pb.RegisterBenchServiceServer(s, svc)
	go func() { _ = s.Serve(ln) }()
	return s, ln.Addr(), nil
}

// DialGRPC dials svc over the transport at addr. If secure, the PQC handshake runs
// (client role) before gRPC sees the conn. The passthrough:/// target keeps
// grpc.NewClient off its default dns resolver.
func DialGRPC(tr Transport, addr string, secure bool, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		c, err := tr.Dial(ctx, addr)
		if err != nil {
			return nil, err
		}
		if secure {
			sc, err := Secure(ctx, c, RoleClient)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			return sc, nil
		}
		return c, nil
	}
	all := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	}, opts...)
	return grpc.NewClient("passthrough:///"+addr, all...)
}
