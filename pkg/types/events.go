// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package types

import (
	"context"
	"errors"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

// ErrRemoteFileNotFound reports that the peer no longer has the file (gRPC NotFound), not a transient fetch failure.
// On-demand reads map it to EOF and map stalls to EIO. It lives here so the filesystem package avoids a gRPC import.
var ErrRemoteFileNotFound = errors.New("remote file not found")

// ErrBatchUnsupported reports that the peer does not implement ReadBatch (gRPC
// Unimplemented). The sibling warmer then stops batching for the session; every
// read uses the per-file path. It lives here for the same no-gRPC-import reason.
var ErrBatchUnsupported = errors.New("peer does not support batched reads")

// FileAction identifies the type of a local filesystem event.
type FileAction int

const (
	Unknown FileAction = iota
	AddDir
	AddFile
	RemoveDir
	RemoveFile
	EditDir
	EditFile
	RenameFile
	RenameDir
	// CancelPendingNotify never reaches the wire: it tells the notify worker
	// to drop a queued ADD/EDIT for the path. Emitted when an acceptance
	// replaces local content — the queued announce describes bytes that no
	// longer exist, and its flush-time attr refresh would stamp the PEER'S
	// content with a fresh local mtime and bounce it back as "newer".
	CancelPendingNotify
)

// FileEvent represents a filesystem change
type FileEvent struct {
	Path    string // Relative path in the FS; the new path for renames.
	OldPath string // Source path for renames.
	Action  FileAction
	Attr    *keibidrop.Attr // File attributes from Stat_t.
	// BaseMtimeNs is the newest peer version the sender accepted when the edit
	// session started. 0 means unknown; the receiver then keeps no conflict copy.
	BaseMtimeNs int64
}

// FileStreamProvider is a factory for RemoteFileStream and StreamFile.
type FileStreamProvider interface {
	OpenRemoteFile(ctx context.Context, inode uint64, path string) (RemoteFileStream, error)
	// StreamFile starts a push-based download. The server sends all chunks
	// from startOffset to EOF without per-chunk round-trips.
	StreamFile(ctx context.Context, path string, startOffset uint64) (StreamFileReceiver, error)
}

type RemoteFileStream interface {
	// ReadAt sends offset and size and receives exactly those bytes.
	ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error)
	Close() error
}

// StreamFileReceiver receives pushed chunks from the server.
type StreamFileReceiver interface {
	// Recv returns the next chunk. It returns io.EOF when the stream ends.
	Recv() (data []byte, offset uint64, totalSize uint64, err error)
}

// ChunkHashReceiver streams (chunkIndex, hash) pairs from a peer's GetChunkHashes RPC.
type ChunkHashReceiver interface {
	Recv() (chunkIndex uint64, hash uint64, err error)
}

// ChunkHasher marks providers that support the GetChunkHashes RPC.
// Callers type-assert and fall back when a provider (older peer, test fake) lacks it.
type ChunkHasher interface {
	GetChunkHashes(ctx context.Context, path string, chunkSize, fromChunk, count uint64) (ChunkHashReceiver, error)
}

// BatchFrame is one ReadBatch frame: a piece of one file in the batch.
type BatchFrame struct {
	Path      string
	Offset    uint64
	Data      []byte
	TotalSize uint64
	FileDone  bool
	ErrCode   uint32
}

// BatchFrameReceiver streams ReadBatch frames. Recv returns io.EOF at the end.
type BatchFrameReceiver interface {
	Recv() (BatchFrame, error)
}

// BatchReader marks providers that support the ReadBatch RPC.
// Callers type-assert and fall back when a provider (older peer, test fake) lacks it.
// An older peer surfaces ErrBatchUnsupported at the first Recv, not at open.
type BatchReader interface {
	ReadBatch(ctx context.Context, paths []string, maxBytes uint64) (BatchFrameReceiver, error)
}
