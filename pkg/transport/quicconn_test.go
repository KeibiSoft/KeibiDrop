// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

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

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/quic-go/quic-go"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "listen")

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
	require.NoError(t, err, "dial")
	cstream, err := cconn.OpenStreamSync(ctx)
	require.NoError(t, err, "open stream")
	return &connPair{client: newQUICConn(cconn, cstream), ln: ln, serverCh: serverCh}
}

// awaitServer returns the server-side net.Conn (blocks until the client's first write).
func (p *connPair) awaitServer(t *testing.T) net.Conn {
	t.Helper()
	select {
	case p.server = <-p.serverCh:
		return p.server
	case <-time.After(5 * time.Second):
		require.True(t, false, "server side never materialized (did the client write?)")
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

	testkit.Run(t, func() error {
		msg := bytes.Repeat([]byte("kd"), 8192) // 16 KiB
		writeErr := testkit.Go(func() error {
			_, err := p.client.Write(msg)
			return err
		})

		server := p.awaitServer(t)
		got := make([]byte, len(msg))
		testkit.Must(io.ReadFull(server, got))
		if err := fp.BytesEqual("client->server payload", got, msg); err != nil {
			return err
		}
		if err := writeErr(); err != nil {
			return err
		}

		// Reverse direction.
		reply := []byte("ack-from-server")
		testkit.Must(server.Write(reply))
		rbuf := make([]byte, len(reply))
		testkit.Must(io.ReadFull(p.client, rbuf))
		return fp.BytesEqual("server->client reply", rbuf, reply)
	})
}

// TestQUICConnReadDeadline proves SetReadDeadline is honored (gRPC relies on
// deadline semantics of the underlying net.Conn).
func TestQUICConnReadDeadline(t *testing.T) {
	p := newConnPair(t)
	defer p.close()

	testkit.Run(t, func() error {
		// Prime the stream so the server side exists, then drain the primer byte.
		go func() { _, _ = p.client.Write([]byte("x")) }()
		server := p.awaitServer(t)
		_, _ = io.ReadFull(server, make([]byte, 1))

		// No further data is coming; a read with a short deadline must time out.
		if err := p.client.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
			return err
		}
		_, readErr := p.client.Read(make([]byte, 8))
		if err := fp.WantErr("read after deadline", readErr); err != nil {
			return err
		}
		var ne net.Error
		isTimeout := errors.Is(readErr, os.ErrDeadlineExceeded) || (errors.As(readErr, &ne) && ne.Timeout())
		return fp.True("read after deadline returns a timeout error", isTimeout)
	})
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

	// Small tolerance for scheduler/GC transients.
	testkit.Eventually(t, 5*time.Second, 25*time.Millisecond, func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+3
	}, "goroutine count settles near baseline after 25 connect/write/close cycles (possible per-conn leak)")
}

func settle(d time.Duration) {
	timer := time.NewTimer(d)
	<-timer.C
}
