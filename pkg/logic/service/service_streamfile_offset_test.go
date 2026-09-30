// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2026 KeibiSoft S.R.L.
// StreamFile at the file's edge: a start offset at EOF is the "already
// complete" resume and streams nothing; one past EOF is a bad request, not an
// empty stream a client could read as a finished zero-byte pull.

//go:build !android

package service

import (
	"os"
	"testing"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/internal/testkit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func streamFileEdgeSvc(t *testing.T) *KeibidropServiceImpl {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "edge-*.bin")
	require.NoError(t, err)
	_, err = f.Write([]byte("0123456789"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return newTestSvc(t, f.Name(), "edge.bin")
}

func TestStreamFile_StartOffsetAtEOFStreamsNothing(t *testing.T) {
	svc := streamFileEdgeSvc(t)
	stream := &testkit.Stream[struct{}, *bindings.StreamFileResponse]{}
	err := svc.StreamFile(&bindings.StreamFileRequest{Path: "edge.bin", StartOffset: 10}, stream)
	require.NoError(t, err)
	assert.Empty(t, stream.Sent)
}

func TestStreamFile_StartOffsetPastEOFIsOutOfRange(t *testing.T) {
	svc := streamFileEdgeSvc(t)
	stream := &testkit.Stream[struct{}, *bindings.StreamFileResponse]{}
	err := svc.StreamFile(&bindings.StreamFileRequest{Path: "edge.bin", StartOffset: 11}, stream)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.OutOfRange, st.Code())
	assert.Empty(t, stream.Sent)
}
