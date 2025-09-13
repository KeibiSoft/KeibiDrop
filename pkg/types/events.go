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
)

// FileEvent represents a filesystem change
type FileEvent struct {
	Path   string          // relative path in the FS
	Action FileAction      // type of event
	Attr   *keibidrop.Attr // file attributes (from Stat_t)
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
		AccessTime:       uint64(stat.Atim.Nsec),
		ModificationTime: uint64(stat.Mtim.Nsec),
		ChangeTime:       uint64(stat.Ctim.Nsec),
		BirthTime:        uint64(stat.Birthtim.Nsec),
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
