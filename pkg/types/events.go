// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package types

import (
	"context"
	"time"

	keibidrop "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/winfsp/cgofuse/fuse"
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

// timespecToNano converts a fuse.Timespec to total nanoseconds since Unix epoch.
// Uses time.Second (1e9 ns) as the constant for seconds-to-nanoseconds conversion.
func timespecToNano(ts fuse.Timespec) uint64 {
	return uint64(ts.Sec)*uint64(time.Second) + uint64(ts.Nsec)
}

// Convert FUSE Stat_t to protobuf Attr
func StatToAttr(stat *fuse.Stat_t) *keibidrop.Attr {
	if stat == nil {
		return nil
	}

	return &keibidrop.Attr{
		Dev:              stat.Dev,
		Ino:              stat.Ino,
		Mode:             stat.Mode,
		Size:             stat.Size,
		AccessTime:       timespecToNano(stat.Atim),
		ModificationTime: timespecToNano(stat.Mtim),
		ChangeTime:       timespecToNano(stat.Ctim),
		BirthTime:        timespecToNano(stat.Birthtim),
		Flags:            stat.Flags,
	}
}

// Convert protobuf Attr back to Stat_t (if needed locally)
func AttrToStat(attr *keibidrop.Attr) *fuse.Stat_t {
	if attr == nil {
		return nil
	}

	return &fuse.Stat_t{
		Dev:      attr.Dev,
		Ino:      attr.Ino,
		Mode:     attr.Mode,
		Size:     attr.Size,
		Atim:     NanoToTimespec(attr.AccessTime),
		Mtim:     NanoToTimespec(attr.ModificationTime),
		Ctim:     NanoToTimespec(attr.ChangeTime),
		Birthtim: NanoToTimespec(attr.BirthTime),
		Flags:    attr.Flags,
	}
}

// Helper to convert nanoseconds to Timespec
func NanoToTimespec(ns uint64) fuse.Timespec {
	return fuse.NewTimespec(time.Unix(0, int64(ns)))
}

// FileStreamProvider is a factory for RemoteFileStream
type FileStreamProvider interface {
	OpenRemoteFile(ctx context.Context, inode uint64, path string) (RemoteFileStream, error)
}

type RemoteFileStream interface {
	// ReadAt sends offset & size, receives exactly those bytes.
	ReadAt(ctx context.Context, offset int64, size int64) ([]byte, error)
	Close() error
}
