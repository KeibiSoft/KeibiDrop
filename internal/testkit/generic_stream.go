// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
//
// ABOUTME: One generic gRPC stream stub, replacing the per-RPC hand-written ones.
// ABOUTME: Also holds the adapters that turn a stream into a plain func.

package testkit

import (
	"context"
	"io"

	"google.golang.org/grpc/metadata"
)

// Stream is a generic server stream stub.
// Req is the received message type, Resp the sent one. Both are concrete at
// every instantiation, so no cast is needed at a call site.
//
// The existing hand-written stubs do not embed grpc.ServerStream. They
// implement the six methods directly, which avoids a nil-interface panic when
// the code under test calls one. This type does the same.
//
// For a send-only RPC, instantiate Req as struct{}. The extra Recv method is
// harmless.
type Stream[Req any, Resp any] struct {
	// Requests are returned by Recv in order, then io.EOF.
	Requests []Req
	// Sent records every message passed to Send.
	Sent []Resp

	// SendErr, when set, is returned by Send instead of recording.
	SendErr error
	// RecvErr, when set, is returned by Recv instead of the next request.
	RecvErr error
	// Ctx overrides the stream context. Nil means context.Background.
	Ctx context.Context

	idx int
}

func (s *Stream[Req, Resp]) Send(resp Resp) error {
	if s.SendErr != nil {
		return s.SendErr
	}
	s.Sent = append(s.Sent, resp)
	return nil
}

func (s *Stream[Req, Resp]) Recv() (Req, error) {
	var zero Req
	if s.RecvErr != nil {
		return zero, s.RecvErr
	}
	if s.idx >= len(s.Requests) {
		return zero, io.EOF
	}
	req := s.Requests[s.idx]
	s.idx++
	return req, nil
}

func (s *Stream[Req, Resp]) Context() context.Context {
	if s.Ctx != nil {
		return s.Ctx
	}
	return context.Background()
}

// The remaining methods satisfy grpc.ServerStream.
// SendMsg and RecvMsg take any because the gRPC interface requires it. This is
// the one place the no-any-as-a-value-type rule does not apply.
func (s *Stream[Req, Resp]) SetHeader(metadata.MD) error  { return nil }
func (s *Stream[Req, Resp]) SendHeader(metadata.MD) error { return nil }
func (s *Stream[Req, Resp]) SetTrailer(metadata.MD)       {}
func (s *Stream[Req, Resp]) SendMsg(any) error            { return nil }
func (s *Stream[Req, Resp]) RecvMsg(any) error            { return nil }

// SendAdapter turns any stream with a Send method into a byte sink.
// The constraint is an anonymous interface, so the binding is compile-time.
func SendAdapter[M any](stream interface{ Send(M) error }, wrap func([]byte) M) func([]byte) error {
	return func(b []byte) error { return stream.Send(wrap(b)) }
}

// RecvAdapter turns any stream with a Recv method into a byte source.
func RecvAdapter[M any](stream interface{ Recv() (M, error) }, unwrap func(M) []byte) func() ([]byte, error) {
	return func() ([]byte, error) {
		m, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		return unwrap(m), nil
	}
}

// Sink collects bytes. Pass Sink.Send to code that takes send func([]byte) error,
// so a test needs no stream, no server and no peer pair.
type Sink struct{ Buf []byte }

func (s *Sink) Send(b []byte) error {
	s.Buf = append(s.Buf, b...)
	return nil
}

// Source returns queued chunks, then io.EOF.
// Pass Source.Recv to code that takes recv func() ([]byte, error).
type Source struct {
	Chunks [][]byte
	idx    int
}

func (s *Source) Recv() ([]byte, error) {
	if s.idx >= len(s.Chunks) {
		return nil, io.EOF
	}
	c := s.Chunks[s.idx]
	s.idx++
	return c, nil
}
