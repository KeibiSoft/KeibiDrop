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
)

// FileEvent represents a filesystem change
type FileEvent struct {
	Path    string // Relative path in the FS; the new path for renames.
	OldPath string // Source path for renames.
	Action  FileAction
	Attr    *keibidrop.Attr // File attributes from Stat_t.
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
