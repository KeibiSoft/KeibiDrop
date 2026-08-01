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

// ErrRemoteFileNotFound is returned by RemoteFileStream.ReadAt when the peer
// reports the file no longer exists (gRPC NotFound), as opposed to a transient
// fetch failure (timeout, connection loss, server I/O error). The on-demand read
// path uses this to distinguish a genuinely gone file (surface EOF, e.g. git's
// transient .keep files) from a stall (surface EIO, so a media player does not
// see a false end-of-file mid-stream). Defined here, in the shared types package,
// so the filesystem package can check it without importing gRPC.
var ErrRemoteFileNotFound = errors.New("remote file not found")

// FileAction maps local FS events
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
	Path    string          // relative path in the FS (new path for renames)
	OldPath string          // for renames: the source path
	Action  FileAction      // type of event
	Attr    *keibidrop.Attr // file attributes (from Stat_t)
	// BaseMtimeNs is the newest peer version the sender had accepted when this
	// edit session started. 0 = unknown; the receiver then never preserves.
	BaseMtimeNs int64
}

// FileStreamProvider is a factory for RemoteFileStream and StreamFile.
type FileStreamProvider interface {
	OpenRemoteFile(ctx context.Context, inode uint64, path string) (RemoteFileStream, error)
	// StreamFile starts a push-based download: server sends all chunks
	// from startOffset to EOF without per-chunk round-trips.
	StreamFile(ctx context.Context, path string, startOffset uint64) (StreamFileReceiver, error)
}

type RemoteFileStream interface {
	// ReadAt sends offset & size, receives exactly those bytes.
	ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error)
	Close() error
}

// StreamFileReceiver receives pushed chunks from the server.
type StreamFileReceiver interface {
	// Recv returns the next chunk. Returns io.EOF when the stream ends.
	Recv() (data []byte, offset uint64, totalSize uint64, err error)
}

// ChunkHashReceiver streams (chunkIndex, hash) pairs from a peer's GetChunkHashes RPC.
type ChunkHashReceiver interface {
	Recv() (chunkIndex uint64, hash uint64, err error)
}

// ChunkHasher is implemented by providers that support the GetChunkHashes RPC. A
// provider that does not implement it (older peer, test fake) signals via the failed
// type assertion at the call site that the caller must fall back.
type ChunkHasher interface {
	GetChunkHashes(ctx context.Context, path string, chunkSize, fromChunk, count uint64) (ChunkHashReceiver, error)
}
