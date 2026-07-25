//go:build bench

// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package transport

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/KeibiSoft/KeibiDrop/pkg/transport/proto"
)

// TestDiagCancelWedge is a diagnostic, not a correctness assertion: it reproduces the
// server-stream cancel wedge over QUIC and reports how many bytes the server sends
// before it blocks. It deliberately never calls srv.Stop (that would hang on the
// wedged handler); process exit reaps the leaked goroutine.
func TestDiagCancelWedge(t *testing.T) {
	if os.Getenv("KD_DIAG") == "" {
		t.Skip("diagnostic harness; set KD_DIAG=1 to run")
	}
	var sent atomic.Uint64
	var done atomic.Bool
	svc := benchService{sent: &sent, done: &done}
	srv, addr, err := ServeGRPC(QUIC(), "127.0.0.1:0", svc, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = srv // intentionally not Stopped

	cc, err := DialGRPC(QUIC(), addr.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	client := pb.NewBenchServiceClient(cc)

	const total = 512 << 10
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := client.Download(ctx, &pb.DownloadRequest{TotalBytes: total, ChunkSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}

	var read uint64
	for i := 0; i < 3; i++ {
		c, err := stream.Recv()
		if err != nil {
			t.Fatalf("recv %d: %v", i, err)
		}
		read += uint64(len(c.Data))
	}
	t.Logf("at cancel: client read=%d, server sent=%d, total=%d", read, sent.Load(), total)
	cancel()

	recvDone := make(chan error, 1)
	go func() { _, e := stream.Recv(); recvDone <- e }()
	select {
	case e := <-recvDone:
		t.Logf("client Recv returned after cancel: %v", e)
	case <-time.After(2 * time.Second):
		t.Logf("client Recv did NOT return within 2s after cancel")
	}

	// Wait for the server to wedge (sent stops advancing below total).
	prev, stable := sent.Load(), 0
	for i := 0; i < 40; i++ {
		time.Sleep(50 * time.Millisecond)
		cur := sent.Load()
		if cur == prev {
			stable++
		} else {
			stable, prev = 0, cur
		}
		if stable >= 6 { // ~300ms no progress
			break
		}
	}
	if done.Load() {
		t.Logf("RESULT=UNWOUND: handler returned on its own at sent=%d (Send saw the cancel)", sent.Load())
		return
	}

	// Handler is wedged in Send: does closing the client connection unblock it, how fast?
	t.Logf("RESULT=WEDGED at sent=%d; closing client connection", sent.Load())
	tClose := time.Now()
	_ = cc.Close()
	for i := 0; i < 160; i++ { // up to 8s
		time.Sleep(50 * time.Millisecond)
		if done.Load() {
			t.Logf(">>> handler RETURNED %v after cc.Close (sent=%d)", time.Since(tClose), sent.Load())
			return
		}
	}
	t.Logf(">>> STILL wedged 8s after cc.Close (sent=%d) — client-close does NOT unblock it", sent.Load())
}
