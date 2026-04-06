// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package types

import (
	"context"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
)

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
)

// FileEvent represents a filesystem change
type FileEvent struct {
	Path    string          // relative path in the FS (new path for renames)
	OldPath string          // for renames: the source path
	Action  FileAction      // type of event
	Attr    *keibidrop.Attr // file attributes (from Stat_t)
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
