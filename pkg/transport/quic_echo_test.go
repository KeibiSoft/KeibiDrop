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
	"testing"
	"time"

	"github.com/KeibiSoft/KeibiDrop/internal/fp"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/quic-go/quic-go"
)

// TestQUICEcho is a smoke test for quic-go's concrete-struct API (*quic.Listener,
// *quic.Conn, *quic.Stream): server echoes bytes on the first stream the client opens,
// client round-trips a message. No gRPC.
func TestQUICEcho(t *testing.T) {
	testkit.Run(t, func() error {
		ln := testkit.Must(quic.ListenAddr("127.0.0.1:0", serverTLSConfig(), nil))
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

		conn := testkit.Must(quic.DialAddr(ctx, ln.Addr().String(), clientTLSConfig(), nil))
		defer conn.CloseWithError(0, "")

		stream := testkit.Must(conn.OpenStreamSync(ctx))

		msg := []byte("keibidrop-quic-echo")
		testkit.Must(stream.Write(msg))

		buf := make([]byte, len(msg))
		testkit.Must(io.ReadFull(stream, buf))
		return fp.BytesEqual("echo", buf, msg)
	})
}
