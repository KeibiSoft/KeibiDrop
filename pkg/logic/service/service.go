package service

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	"github.com/inconshreveable/log15"
	"github.com/winfsp/cgofuse/fuse"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrGRPCNotMounted         = status.Error(codes.FailedPrecondition, "filesystem not mounted")
	ErrGRPCInvalidArgument    = status.Error(codes.InvalidArgument, "invalid argument")
	ErrGRPCFailedPrecondition = status.Error(codes.FailedPrecondition, "failed precondition")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session *session.Session
	Logger  log15.Logger
	FS      *filesystem.FS
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("WAWAWAWAWWEAWEAWEAWEDAEWAWEDAWE")

	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.New("method", "notify", "req-type", req.Type)
	if kd.FS == nil {
		logger.Warn("Filesystem not mounted")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument
	case bindings.NotifyType_ADD_DIR:
		logger.Info("Mkdir called")
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
	logger := kd.Logger.New("method", "read")
	isOpen := false

	var fh *os.File
	size := 0
	offset := 0
	// hardcode buffer to 1^19 (16 MiB)
	buf := make([]byte, 1<<19)
	for {
		rec, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Info("Success")
				return nil
			}
			logger.Error("Failed to recv from stream", "error", err)
			return status.Error(codes.Internal, "failed to stream recv")
		}
		if !isOpen {
			isOpen = true
			kd.FS.Root.AfmLock.RLock()
			f, ok := kd.FS.Root.AllFileMap[rec.Path]
			kd.FS.Root.AfmLock.RUnlock()
			if !ok {
				logger.Warn("Not found", "rec", rec)
				return status.Error(codes.NotFound, "file not found")
			}
			fh, err = os.Open(f.RealPathOfFile)
			if err != nil {
				logger.Error("Failed to open real file", "error", err)
				return status.Error(codes.Internal, "error accesing file")
			}
		}

		n, err := fh.ReadAt(buf[:size], int64(offset))
		if err != nil {
			logger.Error("Failed to read file", "error", err)
			return status.Error(codes.Internal, "error reading file")
		}

		err = stream.Send(&bindings.ReadResponse{
			Data: buf[:n],
		})
		if err != nil {
			if errors.Is(err, io.EOF) {
				logger.Info("Success")
				return nil
			}
			logger.Error("Failed to send read file", "error", err)
			return status.Error(codes.Internal, "failed to send data")
		}
	}
}
