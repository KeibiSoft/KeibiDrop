package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"github.com/winfsp/cgofuse/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrGRPCNotMounted         = status.Error(codes.FailedPrecondition, "filesystem not mounted")
	ErrGRPCInvalidArgument    = status.Error(codes.InvalidArgument, "invalid argument")
	ErrGRPCFailedPrecondition = status.Error(codes.FailedPrecondition, "failed precondition")
	ErrGRPCAlreadyExists      = status.Error(codes.AlreadyExists, "already exists")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session     *session.Session
	Logger      *slog.Logger
	FS          *filesystem.FS
	SyncTracker *synctracker.SyncTracker
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("Debug called. Success!")

	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.With("method", "notify", "req-type", req.Type)

	if kd.FS == nil && kd.SyncTracker == nil {
		logger.Warn("Filesystem not mounted")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument
	case bindings.NotifyType_ADD_DIR:
		logger.Info("Mkdir called")

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS.Root.Mkdir(req.Path, 0777) // Read/Write/Execute for current user.
		if err != 0 {
			return nil, ErrGRPCFailedPrecondition
		}
	case bindings.NotifyType_EDIT_DIR:
		logger.Info("Edit dir is not implemented")
		// TODO: Modify the attr time as per the client payload.
	case bindings.NotifyType_REMOVE_DIR:
		logger.Info("Remove dir is not implemented")
		// TODO: Remove dir as per the client payload.
	case bindings.NotifyType_ADD_FILE:
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCInvalidArgument
		}

		atim := time.Unix(0, int64(req.Attr.AccessTime))
		mtim := time.Unix(0, int64(req.Attr.ModificationTime))
		ctim := time.Unix(0, int64(req.Attr.ChangeTime))
		btim := time.Unix(0, int64(req.Attr.BirthTime))

		if (kd.FS == nil || kd.FS.Root == nil) && kd.SyncTracker != nil {
			kd.SyncTracker.RemoteFilesMu.Lock()
			defer kd.SyncTracker.RemoteFilesMu.Unlock()
			_, ok := kd.SyncTracker.RemoteFiles[req.Path]
			if ok {
				logger.Error("File already exists", "error", ErrGRPCAlreadyExists)
				return nil, ErrGRPCAlreadyExists
			}

			kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
				Name:         req.Name,
				RelativePath: req.Path,
				LastEditTime: req.Attr.ModificationTime,
				CreatedTime:  req.Attr.BirthTime,
			}

			logger.Info("Success")

			return &bindings.NotifyResponse{}, nil
		}

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		logger.Warn("DEBUG ROOT ADD REMOTE FILE")
		err := kd.FS.Root.AddRemoteFile(logger, req.Path, req.Name, &fuse.Stat_t{
			Dev:      req.Attr.Dev,
			Ino:      req.Attr.Ino,
			Mode:     req.Attr.Mode,
			Nlink:    1,
			Size:     req.Attr.Size,
			Atim:     fuse.NewTimespec(atim),
			Mtim:     fuse.NewTimespec(mtim),
			Ctim:     fuse.NewTimespec(ctim),
			Birthtim: fuse.NewTimespec(btim),
			Flags:    req.Attr.Flags,
		})
		if err != nil {
			return nil, ErrGRPCInvalidArgument
		}
	case bindings.NotifyType_EDIT_FILE:
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCFailedPrecondition
		}
		atim := time.Unix(0, int64(req.Attr.AccessTime))
		mtim := time.Unix(0, int64(req.Attr.ModificationTime))
		ctim := time.Unix(0, int64(req.Attr.ChangeTime))
		btim := time.Unix(0, int64(req.Attr.BirthTime))

		if kd.FS == nil {
			logger.Warn("Nil FS")
			return nil, ErrGRPCFailedPrecondition
		}

		if kd.FS.Root == nil {
			logger.Warn("Nil Root FS")
			return nil, ErrGRPCFailedPrecondition
		}

		err := kd.FS.Root.EditRemoteFile(logger, req.Path, req.Name, &fuse.Stat_t{
			Dev:      req.Attr.Dev,
			Ino:      req.Attr.Ino,
			Mode:     req.Attr.Mode,
			Nlink:    1,
			Size:     req.Attr.Size,
			Atim:     fuse.NewTimespec(atim),
			Mtim:     fuse.NewTimespec(mtim),
			Ctim:     fuse.NewTimespec(ctim),
			Birthtim: fuse.NewTimespec(btim),
			Flags:    req.Attr.Flags,
		})

		if err != nil {
			return nil, ErrGRPCFailedPrecondition
		}
	case bindings.NotifyType_REMOVE_FILE:
		logger.Info("Remove file is not implemented")
		// TODO: Remove file as per the client payload.
	}

	logger.Info("Success")

	return &bindings.NotifyResponse{}, nil
}

func (kd *KeibidropServiceImpl) Read(stream bindings.KeibiService_ReadServer) error {
	logger := kd.Logger.With("method", "server-read")

	if (kd.FS == nil || kd.FS.Root == nil) && kd.SyncTracker != nil {
		// f,ok:= kd.SyncTracker.LocalFiles[]
		kd.SyncTracker.LocalFilesMu.RLock()
		defer kd.SyncTracker.LocalFilesMu.RUnlock()

		isOpen := false

		var fh *os.File
		var openedPath string
		// hardcode buffer to 16 MiB (1<<24 is 16 MB; adjust if needed)
		buf := make([]byte, 1<<24)

		for {
			rec, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					if fh != nil {
						fh.Close()
					}
					logger.Info("Read stream finished")
					return nil
				}
				if fh != nil {
					fh.Close()
				}
				logger.Error("Failed to receive from stream", "error", err)
				return status.Error(codes.Internal, "failed to receive read request")
			}

			if !isOpen {
				isOpen = true
				f, ok := kd.SyncTracker.LocalFiles[rec.Path]
				if !ok {
					logger.Warn("File not found", "rec", rec)
					return status.Error(codes.NotFound, "file not found")
				}

				fh, err = os.Open(f.RealPathOfFile)
				if err != nil {
					logger.Error("Failed to open real file", "error", err)
					return status.Error(codes.Internal, "error accessing file")
				}
				openedPath = rec.Path
			} else if openedPath != rec.Path {
				logger.Error("Multiple paths in same stream not supported", "requested", rec.Path)
				return status.Error(codes.InvalidArgument, "stream can only read a single file")
			}

			size := int(rec.Size)
			offset := int64(rec.Offset)
			if size > len(buf) {
				logger.Warn("Requested size too large, truncating to buffer size", "requested", size, "buffer", len(buf))
				size = len(buf)
			}

			n, err := fh.ReadAt(buf[:size], offset)
			if err != nil && !errors.Is(err, io.EOF) {
				logger.Error("Failed to read file", "error", err)
				return status.Error(codes.Internal, "error reading file")
			}

			err = stream.Send(&bindings.ReadResponse{
				Data: buf[:n],
			})
			if err != nil {
				if errors.Is(err, io.EOF) {
					if fh != nil {
						fh.Close()
					}
					logger.Info("Read stream finished")
					return nil
				}
				if fh != nil {
					fh.Close()
				}
				logger.Error("Failed to send read data", "error", err)
				return status.Error(codes.Internal, "failed to send data")
			}
		}
	}

	if kd.FS == nil || kd.FS.Root == nil {
		logger.Error("FS or Root is nil")
		return ErrGRPCFailedPrecondition
	}

	isOpen := false
	kd.FS.Root.AfmLock.RLock()
	defer kd.FS.Root.AfmLock.RUnlock()

	var fh *os.File
	var openedPath string
	// hardcode buffer to 16 MiB (1<<24 is 16 MB; adjust if needed)
	buf := make([]byte, 1<<24)

	for {
		rec, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if fh != nil {
					fh.Close()
				}
				logger.Info("Read stream finished")
				return nil
			}
			if fh != nil {
				fh.Close()
			}
			logger.Error("Failed to receive from stream", "error", err)
			return status.Error(codes.Internal, "failed to receive read request")
		}

		if !isOpen {
			isOpen = true
			f, ok := kd.FS.Root.AllFileMap[rec.Path]
			if !ok {
				logger.Warn("File not found", "rec", rec)
				return status.Error(codes.NotFound, "file not found")
			}

			fh, err = os.Open(f.RealPathOfFile)
			if err != nil {
				logger.Error("Failed to open real file", "error", err)
				return status.Error(codes.Internal, "error accessing file")
			}
			openedPath = rec.Path
		} else if openedPath != rec.Path {
			logger.Error("Multiple paths in same stream not supported", "requested", rec.Path)
			return status.Error(codes.InvalidArgument, "stream can only read a single file")
		}

		size := int(rec.Size)
		offset := int64(rec.Offset)
		if size > len(buf) {
			logger.Warn("Requested size too large, truncating to buffer size", "requested", size, "buffer", len(buf))
			size = len(buf)
		}

		n, err := fh.ReadAt(buf[:size], offset)
		if err != nil && !errors.Is(err, io.EOF) {
			logger.Error("Failed to read file", "error", err)
			return status.Error(codes.Internal, "error reading file")
		}

		err = stream.Send(&bindings.ReadResponse{
			Data: buf[:n],
		})
		if err != nil {
			if errors.Is(err, io.EOF) {
				if fh != nil {
					fh.Close()
				}
				logger.Info("Read stream finished")
				return nil
			}
			if fh != nil {
				fh.Close()
			}
			logger.Error("Failed to send read data", "error", err)
			return status.Error(codes.Internal, "failed to send data")
		}
	}
}
