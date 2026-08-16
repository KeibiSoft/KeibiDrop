//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"io"
	"net"
	"sort"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
	"github.com/stretchr/testify/require"
)

// transport is one of the two wirings under test, QUIC or plain TCP. Identical start
// and dial signatures let every test and benchmark run over both by iterating the table.
type transport struct {
	name  string
	start func(string, pb.BenchServiceServer, ...grpc.ServerOption) (*grpc.Server, net.Addr, error)
	dial  func(string, ...grpc.DialOption) (*grpc.ClientConn, error)
}

var transports = []transport{
	{"quic", startQUICServer, dialQUIC},
	{"tcp", startTCPServer, dialTCP},
}

// Thin wrappers preserving the signatures the tests and benchmarks use.
func startQUICServer(addr string, svc pb.BenchServiceServer, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPC(QUIC(), addr, svc, false, opts...)
}

func startTCPServer(addr string, svc pb.BenchServiceServer, opts ...grpc.ServerOption) (*grpc.Server, net.Addr, error) {
	return ServeGRPC(TCP(), addr, svc, false, opts...)
}

func dialQUIC(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPC(QUIC(), addr, false, opts...)
}

func dialTCP(addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return DialGRPC(TCP(), addr, false, opts...)
}

// newClient starts a server with the default service and connects a client, returning
// the stub and a cleanup func.
func (tr transport) newClient(t testing.TB) (pb.BenchServiceClient, func()) {
	return tr.newClientSvc(t, benchService{})
}

// newClientSvc is newClient with a caller-supplied service, so tests can install hooks.
func (tr transport) newClientSvc(t testing.TB, svc pb.BenchServiceServer) (pb.BenchServiceClient, func()) {
	t.Helper()
	srv, addr, err := tr.start("127.0.0.1:0", svc)
	require.NoError(t, err, "[%s] start server", tr.name)
	cc, err := tr.dial(addr.String())
	if err != nil {
		srv.Stop()
		require.NoError(t, err, "[%s] dial", tr.name)
	}
	return pb.NewBenchServiceClient(cc), func() {
		_ = cc.Close()
		srv.Stop()
	}
}

// verifyRange checks a range read against Read's position-dependent pattern.
func verifyRange(data []byte, offset uint64) bool {
	for i, c := range data {
		if c != byte(offset+uint64(i)) {
			return false
		}
	}
	return true
}

// drainDownload reads a Download stream to EOF (check per chunk if set), returning bytes read and first error.
func drainDownload(stream pb.BenchService_DownloadClient, check func(*pb.Chunk) error) (uint64, error) {
	var got uint64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		if check != nil {
			if err := check(chunk); err != nil {
				return got, err
			}
		}
		got += uint64(len(chunk.Data))
	}
}

// saturateBulk keeps a size-byte Download loop saturating the channel until stop is called.
func saturateBulk(ctx context.Context, client pb.BenchServiceClient, size uint64) (stop func()) {
	bulkCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for bulkCtx.Err() == nil {
			stream, err := client.Download(bulkCtx, &pb.DownloadRequest{TotalBytes: size, ChunkSize: 64 << 10})
			if err != nil {
				return
			}
			_, _ = drainDownload(stream, nil)
		}
	}()
	return func() { cancel(); wg.Wait() }
}

// pctile returns the q-quantile (0..1) of d, sorting a copy.
func pctile(d []time.Duration, q float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(q*float64(len(s)-1))]
}
