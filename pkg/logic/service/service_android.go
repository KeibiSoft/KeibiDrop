// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2025 KeibiSoft S.R.L.
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// ABOUTME: Android-only service implementation. Uses SyncTracker paths only.
// ABOUTME: FUSE/cgofuse paths are excluded — FUSE is not available on Android.

//go:build android

package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	bindings "github.com/KeibiSoft/KeibiDrop/grpc_bindings"
	"github.com/KeibiSoft/KeibiDrop/pkg/config"
	"github.com/KeibiSoft/KeibiDrop/pkg/filesystem"
	"github.com/KeibiSoft/KeibiDrop/pkg/session"
	synctracker "github.com/KeibiSoft/KeibiDrop/pkg/sync-tracker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var (
	ErrGRPCNotMounted         = status.Error(codes.FailedPrecondition, "filesystem not mounted")
	ErrGRPCInvalidArgument    = status.Error(codes.InvalidArgument, "invalid argument")
	ErrGRPCFailedPrecondition = status.Error(codes.FailedPrecondition, "failed precondition")
	ErrGRPCAlreadyExists      = status.Error(codes.AlreadyExists, "already exists")
	ErrGRPCNotFound           = status.Error(codes.NotFound, "notFound")
)

type KeibidropServiceImpl struct {
	bindings.UnimplementedKeibiServiceServer
	Session      *session.Session
	Logger       *slog.Logger
	FS           *filesystem.FS
	SyncTracker  *synctracker.SyncTracker
	OnEvent      func(string)
	OnDisconnect func()
}

func (kd *KeibidropServiceImpl) Debug(context.Context, *bindings.DebugRequest) (*bindings.DebugResponse, error) {
	kd.Logger.Info("Debug called. Success!")
	return &bindings.DebugResponse{}, nil
}

func (kd *KeibidropServiceImpl) Notify(_ context.Context, req *bindings.NotifyRequest) (*bindings.NotifyResponse, error) {
	logger := kd.Logger.With("method", "notify", "req-type", req.Type)

	if req.Type == bindings.NotifyType_DISCONNECT {
		logger.Info("Peer requested graceful disconnect")
		if kd.OnEvent != nil {
			kd.OnEvent("peer_disconnected:")
		}
		if kd.OnDisconnect != nil {
			go kd.OnDisconnect()
		}
		return &bindings.NotifyResponse{Status: "ok"}, nil
	}

	if kd.SyncTracker == nil {
		logger.Warn("SyncTracker not initialized")
		return nil, ErrGRPCNotMounted
	}

	switch req.Type {
	case bindings.NotifyType_UNKNOWN:
		logger.Warn("Unknown notification")
		return nil, ErrGRPCInvalidArgument

	case bindings.NotifyType_ADD_DIR, bindings.NotifyType_EDIT_DIR,
		bindings.NotifyType_REMOVE_DIR, bindings.NotifyType_RENAME_DIR:
		// Directories are not tracked in SyncTracker mode.
		logger.Info("Directory notification ignored in SyncTracker mode", "type", req.Type)

	case bindings.NotifyType_ADD_FILE:
		if req.Attr == nil {
			logger.Error("Failed to add file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCInvalidArgument
		}

		kd.SyncTracker.RemoteFilesMu.Lock()
		defer kd.SyncTracker.RemoteFilesMu.Unlock()

		existing, ok := kd.SyncTracker.RemoteFiles[req.Path]
		if ok {
			existing.Size = uint64(req.Attr.Size)
			existing.LastEditTime = req.Attr.ModificationTime
			logger.Info("Updated existing remote file", "path", req.Path, "newSize", req.Attr.Size)
			return &bindings.NotifyResponse{}, nil
		}

		kd.SyncTracker.RemoteFiles[req.Path] = &synctracker.File{
			Name:         req.Name,
			RelativePath: req.Path,
			Size:         uint64(req.Attr.Size),
			LastEditTime: req.Attr.ModificationTime,
			CreatedTime:  req.Attr.BirthTime,
		}
		logger.Info("Success")

	case bindings.NotifyType_EDIT_FILE:
		if req.Attr == nil {
			logger.Error("Failed to edit file, invalid attr", "error", ErrGRPCInvalidArgument)
			return nil, ErrGRPCFailedPrecondition
		}

		kd.SyncTracker.RemoteFilesMu.RLock()
		defer kd.SyncTracker.RemoteFilesMu.RUnlock()

		f, ok := kd.SyncTracker.RemoteFiles[req.Path]
		if !ok {
			logger.Error("File does not exist", "error", ErrGRPCNotFound)
			return nil, ErrGRPCNotFound
		}

		f.Name = req.Name
		f.RelativePath = req.Path
		f.Size = uint64(req.Attr.Size)
		f.LastEditTime = req.Attr.ModificationTime
		f.CreatedTime = req.Attr.BirthTime
		logger.Info("Success")

	case bindings.NotifyType_REMOVE_FILE:
		logger.Info("Remove file reference", "path", req.Path)
		kd.SyncTracker.RemoteFilesMu.Lock()
		delete(kd.SyncTracker.RemoteFiles, req.Path)
		kd.SyncTracker.RemoteFilesMu.Unlock()
		logger.Info("Removed file from sync tracker", "path", req.Path)

	case bindings.NotifyType_RENAME_FILE:
		logger.Info("Rename file", "oldPath", req.OldPath, "newPath", req.Path)
		kd.SyncTracker.RemoteFilesMu.Lock()
		if f, ok := kd.SyncTracker.RemoteFiles[req.OldPath]; ok {
			delete(kd.SyncTracker.RemoteFiles, req.OldPath)
			f.RelativePath = req.Path
			f.Name = filepath.Base(req.Path)
			kd.SyncTracker.RemoteFiles[req.Path] = f
		}
		kd.SyncTracker.RemoteFilesMu.Unlock()
		logger.Info("Renamed file in sync tracker", "oldPath", req.OldPath, "newPath", req.Path)
	}

	logger.Info("Success")
	return &bindings.NotifyResponse{}, nil
}

func (kd *KeibidropServiceImpl) BatchNotify(ctx context.Context, req *bindings.BatchNotifyRequest) (*bindings.BatchNotifyResponse, error) {
	logger := kd.Logger.With("method", "batch-notify", "count", len(req.Notifications), "seq", req.Seq)
	logger.Info("Processing batch")

	var processed uint32
	for _, n := range req.Notifications {
		_, err := kd.Notify(ctx, n)
		if err != nil {
			logger.Error("Failed to process notification in batch", "path", n.Path, "type", n.Type, "error", err)
			continue
		}
		processed++
	}

	logger.Info("Batch complete", "processed", processed)
	return &bindings.BatchNotifyResponse{Status: "ok", Processed: processed}, nil
}

func (kd *KeibidropServiceImpl) Read(stream bindings.KeibiService_ReadServer) error {
	logger := kd.Logger.With("method", "server-read")

	if kd.SyncTracker == nil {
		logger.Error("SyncTracker not initialized")
		return ErrGRPCFailedPrecondition
	}

	kd.SyncTracker.LocalFilesMu.RLock()
	defer kd.SyncTracker.LocalFilesMu.RUnlock()

	isOpen := false
	var fh *os.File
	var openedPath string
	buf := make([]byte, config.GRPCStreamBuffer)

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
			lookupPath := strings.TrimPrefix(rec.Path, "/")
			f, ok := kd.SyncTracker.LocalFiles[lookupPath]
			if !ok {
				f, ok = kd.SyncTracker.LocalFiles[rec.Path]
			}
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

// StreamFile pushes an entire file's contents to the client in sequential
// chunks. The client sends one request; the server streams all data from
// start_offset to EOF with no per-chunk round-trip overhead.
func (kd *KeibidropServiceImpl) StreamFile(req *bindings.StreamFileRequest, stream bindings.KeibiService_StreamFileServer) error {
	logger := kd.Logger.With("method", "stream-file", "path", req.Path)

	if kd.SyncTracker == nil {
		logger.Warn("SyncTracker not initialized")
		return status.Error(codes.FailedPrecondition, "sync tracker not initialized")
	}

	lookupPath := strings.TrimPrefix(req.Path, "/")
	kd.SyncTracker.LocalFilesMu.RLock()
	f, ok := kd.SyncTracker.LocalFiles[lookupPath]
	if !ok {
		f, ok = kd.SyncTracker.LocalFiles[req.Path]
	}
	kd.SyncTracker.LocalFilesMu.RUnlock()

	if !ok {
		logger.Warn("File not found", "path", req.Path)
		return status.Error(codes.NotFound, "file not found")
	}

	fh, err := os.Open(f.RealPathOfFile)
	if err != nil {
		logger.Error("Failed to open file", "error", err)
		return status.Error(codes.Internal, "error accessing file")
	}
	defer fh.Close()

	finfo, err := fh.Stat()
	if err != nil {
		logger.Error("Failed to stat file", "error", err)
		return status.Error(codes.Internal, "error stat file")
	}
	fileSize := uint64(finfo.Size())

	buf := make([]byte, config.GRPCStreamBuffer)
	chunkSize := uint64(filesystem.ChunkSize)
	offset := req.StartOffset

	logger.Info("StreamFile starting", "fileSize", fileSize, "startOffset", offset)

	for offset < fileSize {
		size := chunkSize
		if offset+size > fileSize {
			size = fileSize - offset
		}

		n, readErr := fh.ReadAt(buf[:size], int64(offset))
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			logger.Error("Failed to read file", "offset", offset, "error", readErr)
			return status.Error(codes.Internal, "error reading file")
		}
		if n == 0 {
			break
		}

		if sendErr := stream.Send(&bindings.StreamFileResponse{
			Data:      buf[:n],
			Offset:    offset,
			TotalSize: fileSize,
		}); sendErr != nil {
			logger.Info("StreamFile send ended", "offset", offset, "error", sendErr)
			return sendErr
		}

		offset += uint64(n)
	}

	logger.Info("StreamFile complete", "bytesSent", offset-req.StartOffset)
	return nil
}

// Rekey handles key rotation requests for forward secrecy.
func (kd *KeibidropServiceImpl) Rekey(_ context.Context, req *bindings.RekeyRequest) (*bindings.RekeyResponse, error) {
	logger := kd.Logger.With("method", "rekey", "epoch", req.Epoch)

	if kd.Session == nil {
		logger.Warn("Session not initialized")
		return nil, status.Error(codes.FailedPrecondition, "session not initialized")
	}

	resp, err := kd.Session.HandleRekeyRequest(req)
	if err != nil {
		logger.Error("Failed to process rekey request", "error", err)
		return nil, status.Error(codes.Internal, "rekey failed")
	}

	logger.Info("Rekey request processed successfully")
	return resp, nil
}

// Heartbeat responds to connection health checks.
func (kd *KeibidropServiceImpl) Heartbeat(_ context.Context, req *bindings.HeartbeatRequest) (*bindings.HeartbeatResponse, error) {
	return &bindings.HeartbeatResponse{
		Timestamp:    uint64(time.Now().UnixNano()),
		ReqTimestamp: req.Timestamp,
		Seq:          req.Seq,
	}, nil
}
